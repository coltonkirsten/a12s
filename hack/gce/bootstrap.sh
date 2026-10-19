#!/usr/bin/env bash
# Creates what a fleet on the GCE driver needs and the driver never creates itself: a VPC and subnet, the firewall
# rules, Cloud NAT for the Controller, the Controller's service account and custom role, a static internal address,
# and the Controller VM (Debian with Docker). Idempotent: an existing resource is left as it is.
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

log "service account $SA and role $ROLE"
exists iam service-accounts describe "$SA" || g iam service-accounts create "$SA_NAME" --display-name "a12s Controller ($FLEET)"
PERMS=compute.instances.create,compute.instances.delete,compute.instances.get,compute.instances.list
PERMS+=,compute.instances.start,compute.instances.stop,compute.instances.setMetadata,compute.instances.setLabels
PERMS+=,compute.instances.setTags,compute.instances.setMachineType,compute.instances.attachDisk
PERMS+=,compute.instances.detachDisk,compute.instances.setServiceAccount,compute.instances.getGuestAttributes
PERMS+=,compute.disks.create,compute.disks.get,compute.disks.list,compute.disks.delete,compute.disks.resize
PERMS+=,compute.disks.use,compute.disks.setLabels,compute.resourcePolicies.use
PERMS+=,compute.subnetworks.get,compute.subnetworks.use,compute.zoneOperations.get,compute.zoneOperations.list
PERMS+=,compute.firewalls.list,compute.machineTypes.list
PERMS+=,iam.serviceAccounts.create,iam.serviceAccounts.delete,iam.serviceAccounts.get,iam.serviceAccounts.actAs
if exists iam roles describe "$ROLE"; then
  g iam roles update "$ROLE" --permissions "$PERMS" > /dev/null
else
  g iam roles create "$ROLE" --title "a12s Controller" --permissions "$PERMS" > /dev/null
fi
# actAs is project-wide: the Controller could run a VM as any account in the project. That is why a fleet gets a
# project of its own, holding only the Controller's and the agents' accounts.
g projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$SA" --role "projects/$PROJECT/roles/$ROLE" \
  --condition None > /dev/null

log "Controller VM $VM ($MACHINE, Debian with Docker)"
if ! exists compute instances describe "$VM" --zone "$ZONE"; then
  g compute instances create "$VM" --zone "$ZONE" --machine-type "$MACHINE" \
    --subnet "$NET" --private-network-ip "$CONTROLLER_IP" --no-address \
    --service-account "$SA" --scopes cloud-platform --tags a12s-controller \
    --image-family debian-12 --image-project debian-cloud --boot-disk-size 30GB --boot-disk-type pd-balanced \
    --shielded-secure-boot --shielded-vtpm --shielded-integrity-monitoring \
    --metadata startup-script='#!/bin/bash
command -v docker > /dev/null || { apt-get update -q && apt-get install -yq docker.io git make; }'
fi

cat <<EOF

Done. On the Controller VM:
  gcloud compute ssh $VM --project $PROJECT --zone $ZONE --tunnel-through-iap
  # install a12s, then:
  mkdir -p ~/.a12s/<fleet> && printf 'driver: gce\ngce:\n  subnetwork: $NET\n' > ~/.a12s/<fleet>/driver.yaml
  sudo usermod -aG docker \$USER   # then log in again
  a12s up
EOF
