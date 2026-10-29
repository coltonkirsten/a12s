#!/usr/bin/env bash
# Creates what a fleet on the GCE driver needs and the driver never creates itself: a VPC and subnet, the firewall
# rules, Cloud NAT for the Controller, the Controller's service account and custom roles, the sa-tier=infra tag, a
# static internal address, and the Controller VM (Debian with Docker).
# Idempotent: an existing resource is left as it is (custom roles are updated to the permissions below).
# Usage: hack/gce/bootstrap.sh <project> <fleet> [region] [zone] [controller-machine-type]
# Needs gcloud logged in as a principal that can enable services, create those resources and set the project's IAM
# policy (roles/owner on a project made for the fleet is simplest). See docs/drivers/gce.md.
set -euo pipefail

PROJECT=${1:?project}
FLEET=${2:?fleet}
REGION=${3:-us-central1}
ZONE=${4:-$REGION-a}
MACHINE=${5:-e2-small}
SUBNET_RANGE=${A12S_GCE_SUBNET_RANGE:-10.20.0.0/24}
CONTROLLER_IP=${A12S_GCE_CONTROLLER_IP:-10.20.0.2}

NET=a12s-$FLEET
SA_NAME=a12s-controller
SA=$SA_NAME@$PROJECT.iam.gserviceaccount.com
ROLE=a12sController
ID_ROLE=a12sControllerIdentities
VM=a12s-$FLEET-controller

g() { gcloud --project "$PROJECT" --quiet "$@"; }
exists() { g "$@" > /dev/null 2>&1; }
log() { printf '%s\n' "== $*"; }

log "services"
g services enable compute.googleapis.com iam.googleapis.com iap.googleapis.com cloudresourcemanager.googleapis.com

log "network $NET and subnet $NET ($SUBNET_RANGE in $REGION)"
exists compute networks describe "$NET" || g compute networks create "$NET" --subnet-mode=custom
exists compute networks subnets describe "$NET" --region "$REGION" ||
  g compute networks subnets create "$NET" --network "$NET" --region "$REGION" --range "$SUBNET_RANGE"

log "Controller address $CONTROLLER_IP"
exists compute addresses describe "$VM" --region "$REGION" ||
  g compute addresses create "$VM" --region "$REGION" --subnet "$NET" --addresses "$CONTROLLER_IP"

# Agent computer VMs (tag a12s-computer) may send only to the Controller's proxy and mTLS ports; only the Controller
# may reach their engine and computerd. GCE rules can't mix service accounts and tags, so the Controller is named by
# its address.
fw() { exists compute firewall-rules describe "$1" || g compute firewall-rules create "$@" --network "$NET"; }
log "firewall rules"
fw "$NET-computers-deny-egress" --direction EGRESS --action DENY --rules all \
  --destination-ranges 0.0.0.0/0 --target-tags a12s-computer --priority 1000
fw "$NET-computers-to-controller" --direction EGRESS --action ALLOW --rules tcp:3128,tcp:7443 \
  --destination-ranges "$CONTROLLER_IP/32" --target-tags a12s-computer --priority 900
fw "$NET-controller-to-computers" --direction INGRESS --action ALLOW --rules tcp:2376,tcp:7443 \
  --source-ranges "$CONTROLLER_IP/32" --target-tags a12s-computer --priority 1000
fw "$NET-computers-to-controller-in" --direction INGRESS --action ALLOW --rules tcp:3128,tcp:7443 \
  --source-tags a12s-computer --target-tags a12s-controller --priority 1000
fw "$NET-iap-ssh" --direction INGRESS --action ALLOW --rules tcp:22 \
  --source-ranges 35.235.240.0/20 --target-tags a12s-controller --priority 1000

log "Cloud NAT (the Controller's way out; agent computers have none past the firewall)"
exists compute routers describe "$NET" --region "$REGION" || g compute routers create "$NET" --network "$NET" --region "$REGION"
exists compute routers nats describe "$NET" --router "$NET" --region "$REGION" ||
  g compute routers nats create "$NET" --router "$NET" --region "$REGION" \
    --nat-custom-subnet-ip-ranges "$NET" --auto-allocate-nat-external-ips

log "service account $SA"
exists iam service-accounts describe "$SA" || g iam service-accounts create "$SA_NAME" --display-name "a12s Controller ($FLEET)"

# The tag sa-tier=infra marks every service account that is not an agent's: the Controller may not run a VM as one or
# delete it (the conditioned binding below). Tag your app's own accounts with it too; the Controller's is tagged here.
# Tags, not names: IAM Conditions can't match a service account's name for actAs, but they can match its tags.
log "tag $PROJECT/sa-tier=infra on $SA"
exists resource-manager tags keys describe "$PROJECT/sa-tier" ||
  g resource-manager tags keys create sa-tier --parent "projects/$PROJECT" \
    --description "Service account tier. infra: not an agent's; out of reach of the a12s Controller's actAs."
exists resource-manager tags values describe "$PROJECT/sa-tier/infra" ||
  g resource-manager tags values create infra --parent "$PROJECT/sa-tier" --description "Not an agent's account."
# A new service account takes a while to reach other services ("not found"), so tagging retries.
for i in $(seq 1 30); do
  SA_ID=$(g iam service-accounts describe "$SA" --format 'value(uniqueId)')
  SA_PARENT=//iam.googleapis.com/projects/$PROJECT/serviceAccounts/$SA_ID
  g resource-manager tags bindings list --parent "$SA_PARENT" --format 'value(tagValueNamespacedName)' 2> /dev/null |
    grep -qx "$PROJECT/sa-tier/infra" && break
  g resource-manager tags bindings create --parent "$SA_PARENT" --tag-value "$PROJECT/sa-tier/infra" \
    > /dev/null 2>&1 && break
  [ "$i" -lt 30 ] || { echo "could not tag $SA" >&2; exit 1; }
  sleep 5
done

# role <id> <title> <condition file or ""> <permissions>: create or update a custom role and bind it to the Controller.
# A deleted custom role stays (deleted) for 7 days and its ID can't be reused, so a fleet made again within a week
# (after hack/gce/teardown.sh) undeletes it.
role() {
  local id=$1 title=$2 cond=$3 perms=$4 i
  if [ "$(g iam roles describe "$id" --format 'value(deleted)' 2> /dev/null)" = True ]; then
    g iam roles undelete "$id" > /dev/null
  fi
  if exists iam roles describe "$id"; then
    g iam roles update "$id" --permissions "$perms" > /dev/null
  else
    g iam roles create "$id" --title "$title" --permissions "$perms" > /dev/null
  fi
  local c=(--condition None)
  [ -n "$cond" ] && c=(--condition-from-file "$cond")
  # A new service account takes a while to reach IAM policy checks ("does not exist"), so the binding retries.
  for i in $(seq 1 30); do
    g projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$SA" --role "projects/$PROJECT/roles/$id" \
      "${c[@]}" > /dev/null 2> /tmp/a12s-bootstrap-iam && break
    grep -q 'does not exist' /tmp/a12s-bootstrap-iam && [ "$i" -lt 30 ] || { cat /tmp/a12s-bootstrap-iam >&2; exit 1; }
    sleep 5
  done
}

# actAs and delete, for per-computer identity: project-wide (an agent's account name can't be known in advance) but
# never on an account tagged sa-tier=infra. Bound before the main role drops them, so a re-run never leaves a gap.
log "role $ID_ROLE (accounts not tagged sa-tier=infra)"
COND=$(mktemp)
cat > "$COND" << EOF
title: not-infra-service-accounts
description: Not service accounts tagged sa-tier=infra
expression: "!resource.matchTag('$PROJECT/sa-tier', 'infra')"
EOF
role "$ID_ROLE" "a12s Controller: agents' identities" "$COND" iam.serviceAccounts.actAs,iam.serviceAccounts.delete
rm -f "$COND"

log "role $ROLE"
PERMS=compute.instances.create,compute.instances.delete,compute.instances.get,compute.instances.list
PERMS+=,compute.instances.start,compute.instances.stop,compute.instances.setMetadata,compute.instances.setLabels
PERMS+=,compute.instances.setTags,compute.instances.setMachineType,compute.instances.attachDisk
PERMS+=,compute.instances.detachDisk,compute.instances.setServiceAccount,compute.instances.getGuestAttributes
PERMS+=,compute.disks.create,compute.disks.get,compute.disks.list,compute.disks.delete,compute.disks.resize
PERMS+=,compute.disks.use,compute.disks.setLabels,compute.resourcePolicies.use
PERMS+=,compute.subnetworks.get,compute.subnetworks.use,compute.zoneOperations.get,compute.zoneOperations.list
PERMS+=,compute.firewalls.list,compute.machineTypes.list
PERMS+=,iam.serviceAccounts.create,iam.serviceAccounts.get
role "$ROLE" "a12s Controller" "" "$PERMS"

# Debian 13: its docker.io is new enough for BuildKit (docker-buildx), which the images' Dockerfiles need.
log "Controller VM $VM ($MACHINE, Debian with Docker)"
if ! exists compute instances describe "$VM" --zone "$ZONE"; then
  g compute instances create "$VM" --zone "$ZONE" --machine-type "$MACHINE" \
    --subnet "$NET" --private-network-ip "$CONTROLLER_IP" --no-address \
    --service-account "$SA" --scopes cloud-platform --tags a12s-controller \
    --image-family debian-13 --image-project debian-cloud --boot-disk-size 30GB --boot-disk-type pd-balanced \
    --shielded-secure-boot --shielded-vtpm --shielded-integrity-monitoring \
    --metadata startup-script='#!/bin/bash
command -v docker > /dev/null || { apt-get update -q && apt-get install -yq docker.io docker-buildx git make; }'
fi

cat <<EOF

Done. On the Controller VM:
  gcloud compute ssh $VM --project $PROJECT --zone $ZONE --tunnel-through-iap
  # install a12s, then:
  mkdir -p ~/.a12s/<fleet> && printf 'driver: gce\ngce:\n  subnetwork: $NET\n' > ~/.a12s/<fleet>/driver.yaml
  sudo usermod -aG docker \$USER   # then log in again
  a12s up
EOF
