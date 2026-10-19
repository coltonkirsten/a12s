#!/usr/bin/env bash
# shellcheck disable=SC2129 # each step appends to the log as it happens
# The exit gate on a real GCE project. From the operator's machine it bootstraps the fleet project
# (hack/gce/bootstrap.sh), ships this checkout to the Controller VM, and runs hack/gce/vm-gate.sh's steps there: the
# build, hack/exit-gate.sh against the GCE driver, wake latency, the in-VM firewall, per-computer identity in Cloud
# Audit Logs, and a sizing measurement. Then it takes the fleet down and deletes what bootstrap made.
# Usage: hack/gce/gate.sh <project> <logfile> [region] [zone]
# Needs: gcloud logged in with the roles in docs/drivers/gce.md#running-the-exit-gate, GH_TOKEN, ~/.a12s/anthropic and
# ~/.a12s/lab/openai.json (as hack/exit-gate.sh). A12S_GCE_WAKE_P95 (seconds, default 90) is the accepted wake p95;
# A12S_GCE_KEEP=1 leaves the project's resources up afterwards.
set -uo pipefail
PROJECT=${1:?project}; LOG=${2:?logfile}; REGION=${3:-us-central1}; ZONE=${4:-$REGION-a}
: > "$LOG"
cd "$(dirname "$0")/../.." || exit 2
FLEET=gate
VM=a12s-$FLEET-controller
PASS=0; FAIL=0
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }
check() { local name=$1; shift; if "$@" >> "$LOG" 2>&1; then PASS=$((PASS+1)); log "PASS $name"; else FAIL=$((FAIL+1)); log "FAIL $name"; fi; }
g() { gcloud --project "$PROJECT" --quiet "$@"; }
on_vm() { g compute ssh "$VM" --zone "$ZONE" --tunnel-through-iap --command "$*"; }
step() {
  local name=$1; shift
  on_vm "A12S_GCE_WAKE_P95=${A12S_GCE_WAKE_P95:-90} GH_TOKEN=$(printf %q "${GH_TOKEN:-}") a12s/hack/gce/vm-gate.sh /tmp/vm-gate.$name.log $name $*"
  local rc=$?
  g compute scp --zone "$ZONE" --tunnel-through-iap "$VM:/tmp/vm-gate.$name.log" "$LOG.$name" > /dev/null 2>&1 && cat "$LOG.$name" >> "$LOG"
  check "vm step $name" test "$rc" -eq 0
}

log "== GCE exit gate: project $PROJECT, $ZONE, a12s $(git rev-parse --short HEAD) =="
check "bootstrap" hack/gce/bootstrap.sh "$PROJECT" "$FLEET" "$REGION" "$ZONE" e2-standard-4
for _ in $(seq 1 60); do on_vm 'command -v docker && command -v make' > /dev/null 2>&1 && break; sleep 10; done
check "Controller VM has Docker" on_vm 'command -v docker'
# shellcheck disable=SC2016 # $USER expands on the VM
on_vm 'sudo usermod -aG docker $USER; rm -rf a12s; mkdir -p a12s ~/.a12s/lab' >> "$LOG" 2>&1
git archive --format=tar HEAD | g compute ssh "$VM" --zone "$ZONE" --tunnel-through-iap --command 'tar -x -C a12s' >> "$LOG" 2>&1
g compute scp --zone "$ZONE" --tunnel-through-iap ~/.a12s/anthropic "$VM:.a12s/anthropic" >> "$LOG" 2>&1
g compute scp --zone "$ZONE" --tunnel-through-iap ~/.a12s/lab/openai.json "$VM:.a12s/lab/openai.json" >> "$LOG" 2>&1

step build
step exitgate
step latency
step egress

BUCKET=a12s-gate-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
# The app's step, not a12s's: grant the agent's account a role, here on the project for a bucket it creates.
SA=$(on_vm 'a12s/bin/a12s status --json -F a12s/examples/lab/fleet.yaml' 2> /dev/null | python3 -c "
import json, sys
print(next(c.get('identity', '') for c in json.load(sys.stdin)['agentComputers'] if c['name'] == 'scout'))")
log "scout acts as ${SA:-nothing}"
check "grant scout's account storage.admin" g projects add-iam-policy-binding "$PROJECT" --member "serviceAccount:$SA" \
  --role roles/storage.admin --condition None
sleep 60 # IAM propagation
on_vm "sed -i 's/egress: { allow: \[api.anthropic.com\] }/egress: { allow: [api.anthropic.com, storage.googleapis.com] }/' a12s/examples/lab/fleet.yaml && a12s/bin/a12s up -F a12s/examples/lab/fleet.yaml" >> "$LOG" 2>&1
step identity "$BUCKET"
audited() {
  for _ in $(seq 1 30); do
    g logging read "protoPayload.methodName=\"storage.buckets.create\" AND protoPayload.resourceName:\"$BUCKET\" AND protoPayload.authenticationInfo.principalEmail=\"$SA\"" \
      --freshness 1h --format 'value(protoPayload.authenticationInfo.principalEmail)' | grep -q "$SA" && return 0
    sleep 10
  done
  return 1
}
check "Cloud Audit Logs name scout's account for its bucket" audited
g storage rm --recursive "gs://$BUCKET" >> "$LOG" 2>&1
g projects remove-iam-policy-binding "$PROJECT" --member "serviceAccount:$SA" --role roles/storage.admin --condition None >> "$LOG" 2>&1
on_vm 'cd a12s && cp examples/lab/fleet.yaml.gate examples/lab/fleet.yaml && bin/a12s up -F examples/lab/fleet.yaml' >> "$LOG" 2>&1

step sizing
step down
if [ "${A12S_GCE_KEEP:-0}" != 1 ]; then
  check "teardown" hack/gce/teardown.sh "$PROJECT" "$FLEET" "$REGION" "$ZONE"
fi
log "== RESULT: $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
