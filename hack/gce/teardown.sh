#!/usr/bin/env bash
# Deletes what hack/gce/bootstrap.sh made, and any a12s VMs and disks of the project's fleets that a12s down left.
# Usage: hack/gce/teardown.sh <project> <fleet> [region] [zone]
set -uo pipefail
PROJECT=${1:?project}; FLEET=${2:?fleet}; REGION=${3:-us-central1}; ZONE=${4:-$REGION-a}
NET=a12s-$FLEET
SA=a12s-controller@$PROJECT.iam.gserviceaccount.com
g() { gcloud --project "$PROJECT" --quiet "$@"; }
rc=0

for vm in $(g compute instances list --filter "labels.a12s-fleet:*" --format 'value(name)'); do
  g compute instances delete "$vm" --zone "$ZONE" || rc=1
done
g compute instances delete "$NET-controller" --zone "$ZONE" 2> /dev/null
for disk in $(g compute disks list --filter "labels.a12s-fleet:*" --format 'value(name)'); do
  g compute disks delete "$disk" --zone "$ZONE" || rc=1
done
for fw in computers-deny-egress computers-to-controller controller-to-computers computers-to-controller-in iap-ssh; do
  g compute firewall-rules delete "$NET-$fw" 2> /dev/null
done
g compute routers nats delete "$NET" --router "$NET" --region "$REGION" 2> /dev/null
g compute routers delete "$NET" --region "$REGION" 2> /dev/null
g compute addresses delete "$NET-controller" --region "$REGION" 2> /dev/null
g compute networks subnets delete "$NET" --region "$REGION" 2> /dev/null
g compute networks delete "$NET" 2> /dev/null || { echo "network $NET still exists"; rc=1; }
# The binding goes before the account: deleting the account first leaves a "deleted:serviceAccount:" member behind.
for role in a12sController a12sControllerIdentities; do
  g projects remove-iam-policy-binding "$PROJECT" --member "serviceAccount:$SA" --role "projects/$PROJECT/roles/$role" \
    --all > /dev/null 2>&1
done
for sa in $(g iam service-accounts list --format 'value(email)' --filter "displayName~'^a12s '"); do
  g iam service-accounts delete "$sa" 2> /dev/null
done
g iam service-accounts delete "$SA" 2> /dev/null
g iam roles delete a12sController > /dev/null 2>&1
g iam roles delete a12sControllerIdentities > /dev/null 2>&1
# The sa-tier tag key stays: your app's own service accounts may carry it.
exit "$rc"
