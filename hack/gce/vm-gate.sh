#!/usr/bin/env bash
# The GCE exit gate's steps on the Controller VM; hack/gce/gate.sh runs them from the operator's machine.
# Usage: hack/gce/vm-gate.sh <logfile> <step> [args]
#   build                    build the images, bundles, bin/a12s and bin/probe from this checkout
#   exitgate                 hack/exit-gate.sh against the GCE driver (examples/lab with per-computer identity)
#   latency                  wake-to-Ready p50/p95 for scout over 5 wakes, against A12S_GCE_WAKE_P95 (seconds, default 90)
#   egress                   from inside scout: only the Controller's proxy gets out; no DNS, no public IP, no host
#   identity <bucket>        scout creates a bucket with its own account's token (gate.sh checks the audit log)
#   sizing                   memory, CPU time and disk of an e2-micro and an e2-small agent computer, idle and busy
#   down                     a12s down --purge --yes
set -uo pipefail
LOG=${1:?logfile}; STEP=${2:?step}; shift 2
cd "$(dirname "$0")/../.." || exit 2
REPO=$PWD
A12S=$REPO/bin/a12s
PASS=0; FAIL=0
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }
check() { local name=$1; shift; if "$@" >> "$LOG" 2>&1; then PASS=$((PASS+1)); log "PASS $name"; else FAIL=$((FAIL+1)); log "FAIL $name"; fi; }
# shellcheck disable=SC2329 # called through check
fails() { ! "$@"; }
vm_field() {
  gcloud compute instances describe "a12s-lab-$1" --zone "$(curl -fsS -H 'Metadata-Flavor: Google' \
    http://169.254.169.254/computeMetadata/v1/instance/zone | sed 's|.*/||')" --format="$2"
}
vm_ip() { vm_field "$1" 'value(networkInterfaces[0].networkIP)'; }
probe() { sudo HOME="$HOME" "$REPO/bin/probe" lab "$@"; }
phase() {
  "$A12S" status --json -F examples/lab/fleet.yaml | python3 -c "
import json, sys
d = json.load(sys.stdin)
print(next((c['phase'] for c in d['agentComputers'] if c['name'] == sys.argv[1]), ''))" "$1"
}
wait_phase() { for _ in $(seq 1 "${3:-300}"); do [ "$(phase "$1")" = "$2" ] && return 0; sleep 1; done; return 1; }
finish() { log "== $STEP: $PASS passed, $FAIL failed =="; [ "$FAIL" -eq 0 ]; exit $?; }

case $STEP in
build)
  log "== build on $(docker version --format '{{.Server.Version}} {{.Server.Arch}}') =="
  check "build images" make images
  check "build bundles" make bundles BUNDLE_CLAUDE_VERSIONS="2.1.280 2.1.281" BUNDLE_CODEX_VERSIONS=0.154.0
  GOVERSION=$(sed -n 's/^go \([0-9]*\.[0-9]*\).*/\1/p' go.mod)
  check "build a12s and probe" docker run --rm -v "$REPO:/src" -w /src -e CGO_ENABLED=0 -u "$(id -u):$(id -g)" \
    -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath "golang:$GOVERSION" \
    sh -c 'go build -o bin/a12s ./cmd/a12s && go build -o bin/probe ./hack/gce/probe'
  cp examples/lab/fleet.yaml examples/lab/fleet.yaml.gate
  mkdir -p ~/.a12s/lab
  printf 'driver: gce\ngce:\n  subnetwork: %s\n  identity: per-computer\n' "${A12S_GCE_SUBNETWORK:-a12s-gate}" > ~/.a12s/lab/driver.yaml
  finish
  ;;
exitgate)
  hack/exit-gate.sh "$LOG.exit-gate"
  rc=$?
  cat "$LOG.exit-gate" >> "$LOG"
  check "exit gate on GCE" test "$rc" -eq 0
  finish
  ;;
latency)
  THRESHOLD=${A12S_GCE_WAKE_P95:-90}
  : > /tmp/gce-wakes
  for i in 1 2 3 4 5; do
    "$A12S" sleep scout -F examples/lab/fleet.yaml >> "$LOG" 2>&1
    wait_phase scout Asleep 300 || { log "scout did not sleep (wake $i)"; continue; }
    t0=$(date +%s.%N)
    "$A12S" wake scout -F examples/lab/fleet.yaml >> "$LOG" 2>&1
    if wait_phase scout Ready 600; then
      awk -v t0="$t0" -v t1="$(date +%s.%N)" 'BEGIN { print t1 - t0 }' >> /tmp/gce-wakes
      log "wake $i: $(tail -1 /tmp/gce-wakes) s"
    else
      log "wake $i: scout not Ready after 600 s"
    fi
  done
  python3 - "$THRESHOLD" /tmp/gce-wakes > /tmp/gce-latency <<'PY'
import statistics, sys
w = sorted(float(l) for l in open(sys.argv[2]) if l.strip())
if len(w) < 5:
    print(f"only {len(w)} of 5 wakes reached Ready"); sys.exit(1)
p50, p95 = statistics.median(w), w[min(len(w) - 1, round(0.95 * (len(w) - 1)))]
print(f"wake to Ready over {len(w)} wakes: p50 {p50:.1f} s, p95 {p95:.1f} s, threshold {sys.argv[1]} s")
sys.exit(0 if p95 <= float(sys.argv[1]) else 1)
PY
  rc=$?
  tee -a "$LOG" < /tmp/gce-latency
  check "wake p95 within ${THRESHOLD}s" test "$rc" -eq 0
  finish
  ;;
egress)
  # Positive control first: the allowed host through the proxy works, so a failure below is the firewall's.
  # shellcheck disable=SC2016 # $A12S_PROXY expands inside the agent computer
  check "scout reaches an allowed host through the proxy" \
    probe exec scout sh -c 'curl -sS -m 15 -o /dev/null -w "%{http_code}\n" -x "$A12S_PROXY" https://api.anthropic.com/'
  check "scout can't reach a public IP directly" fails probe exec scout curl -sS -m 5 -o /dev/null https://1.1.1.1/
  check "scout can't resolve names (no DNS, not even the metadata server's)" \
    fails probe exec scout python3 -c 'import socket; socket.getaddrinfo("google.com", 443)'
  scout_ip=$(vm_ip scout); builder_ip=$(vm_ip builder)
  check "scout can't reach its VM's engine" fails probe exec scout curl -sk -m 5 "https://$scout_ip:2376/_ping"
  check "scout can't reach its VM through the bridge" fails probe exec scout curl -sk -m 5 https://172.17.0.1:2376/_ping
  check "scout can't reach another agent computer's VM" fails probe exec scout curl -sk -m 5 "https://$builder_ip:7443/"
  finish
  ;;
identity)
  BUCKET=${1:?bucket}
  wait_phase scout Ready 600
  PROJECT=$(curl -fsS -H 'Metadata-Flavor: Google' http://169.254.169.254/computeMetadata/v1/project/project-id)
  # The token comes straight from the metadata server, as Google's libraries get it; the API call goes through the
  # egress proxy like any other.
  check "scout creates $BUCKET as itself" probe exec scout sh -c "
    tok=\$(curl -fsS -m 10 -H 'Metadata-Flavor: Google' http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"access_token\"])')
    curl -fsS -m 30 -x \"\$A12S_PROXY\" -X POST -H \"Authorization: Bearer \$tok\" -H 'Content-Type: application/json' \
      -d '{\"name\":\"$BUCKET\"}' 'https://storage.googleapis.com/storage/v1/b?project=$PROJECT' > /dev/null"
  "$A12S" status --json -F examples/lab/fleet.yaml | python3 -c "
import json, sys
print(next(c.get('identity', '') for c in json.load(sys.stdin)['agentComputers'] if c['name'] == 'scout'))" > /tmp/gce-identity
  check "scout has an identity in status" test -s /tmp/gce-identity
  log "scout's identity: $(cat /tmp/gce-identity)"
  finish
  ;;
sizing)
  cat >> examples/lab/fleet.yaml <<'Y'

  micro:
    harness: { kind: claude-code, version: 2.1.280, model: haiku }
    image: slim
    resources: { cpu: 250m, memory: 1Gi }
    credentials: [anthropic]
    egress: { allow: [api.anthropic.com, github.com, codeload.github.com, registry.npmjs.org] }
  small:
    harness: { kind: claude-code, version: 2.1.280, model: haiku }
    image: slim
    resources: { cpu: 500m, memory: 2Gi }
    credentials: [anthropic]
    egress: { allow: [api.anthropic.com, github.com, codeload.github.com, registry.npmjs.org] }
Y
  "$A12S" up -F examples/lab/fleet.yaml >> "$LOG" 2>&1
  for c in micro small; do
    wait_phase "$c" Ready 600 || { log "FAIL $c not Ready"; FAIL=$((FAIL+1)); continue; }
    log "$c on $(vm_field "$c" 'value(machineType.basename())') idle: $(probe stats "$c")"
    : > "/tmp/gce-peak.$c"
    t0=$(date +%s)
    "$A12S" send --wait "$c" -F examples/lab/fleet.yaml "Use your Bash tool to run exactly: cd /tmp && git clone --depth 1 https://github.com/coltonkirsten/a12s a && rg -c func a | wc -l && cd a/sdk/typescript && npm ci --ignore-scripts --no-audit --no-fund > /dev/null && echo ok. Then reply with the single word done." > "/tmp/gce-busy.$c" 2>&1 &
    pid=$!
    while kill -0 "$pid" 2> /dev/null; do probe stats "$c" >> "/tmp/gce-peak.$c" 2> /dev/null; sleep 5; done
    wait "$pid"; rc=$?
    log "$c busy Turn: $(( $(date +%s) - t0 )) s, exit $rc; peak: $(python3 -c "
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
print(max(rows, key=lambda r: r['memoryBytes']) if rows else 'no samples')" "/tmp/gce-peak.$c")"
    check "$c finished a busy Turn" grep -q "done" "/tmp/gce-busy.$c"
  done
  log "VM memory (e2-micro has 1 GiB, e2-small 2 GiB); boot disk 10 GiB: images and volumes above must fit with room"
  cp examples/lab/fleet.yaml.gate examples/lab/fleet.yaml
  "$A12S" up -F examples/lab/fleet.yaml >> "$LOG" 2>&1
  finish
  ;;
down)
  "$A12S" down --purge --yes -F examples/lab/fleet.yaml >> "$LOG" 2>&1
  check "a12s down --purge" test $? -eq 0
  finish
  ;;
*)
  echo "unknown step $STEP" >&2; exit 2
  ;;
esac
