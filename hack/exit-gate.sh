#!/usr/bin/env bash
# The §8 exit gate, run against the local Docker engine with the examples/lab Fleet.
# Usage: hack/exit-gate.sh <logfile>. Requires: built bin/a12s and a12s/* images,
# examples/lab/a12s.bindings.yaml, and GH_TOKEN for the builder's github credential.
set -uo pipefail
LOG=${1:?logfile}; : > "$LOG"
cd "$(dirname "$0")/../examples/lab"
A12S=../../bin/a12s
PASS=0; FAIL=0
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }
run() { log "\$ $*"; "$@" 2>&1 | tee -a "$LOG"; return "${PIPESTATUS[0]}"; }
check() { if eval "$2"; then PASS=$((PASS+1)); log "PASS $1"; else FAIL=$((FAIL+1)); log "FAIL $1"; fi; }

log "== exit gate on $(docker version --format '{{.Server.Version}} {{.Server.Arch}}') =="
cp fleet.yaml fleet.yaml.orig

log "## 1 one file: four Computers, both harnesses, slim and browser"
check "1 fleet declares four computers" "[ \$(grep -cE '^  (scout|builder|tester|reviewer):' fleet.yaml) -eq 4 ]"

log "## 2 one command: a12s up brings all four to Ready"
run $A12S up; run $A12S status
check "2 all four Ready" "[ \$($A12S status | grep -cE '^(scout|builder|tester|reviewer) +Ready') -eq 4 ]"

log "## 3 streamed reply for each of the four"
for c in scout builder tester reviewer; do
  out=$(run $A12S prompt $c "Reply with the single word pong.")
  check "3 $c streams text to turn.finished" "grep -q pong <<<'$out' && grep -q 'completed' <<<'$out'"
done

log "## 4 sessions resume across sleep --all / wake --all"
for c in scout builder tester reviewer; do run $A12S prompt $c "Remember the secret word: tangerine-$c. Reply only: ok."; done
run $A12S sleep --all; run $A12S status
check "4 all Asleep" "[ \$($A12S status | grep -cE '^(scout|builder|tester|reviewer) +Asleep') -eq 4 ]"
run $A12S wake --all; run $A12S status
for c in scout builder tester reviewer; do
  out=$(run $A12S prompt $c "What was the secret word I asked you to remember? Reply with just the word.")
  check "4 $c resumed: true and recalls the word" "grep -q 'resumed: true' <<<'$out' && grep -q \"tangerine-$c\" <<<'$out'"
done

log "## 5 scaling is editing: add a fifth, only it is created; remove it, drives kept"
cat >> fleet.yaml <<'Y'

  fifth:
    harness: { kind: codex, version: 0.154.0 }
    image: slim
    credentials: [openai]
    egress: { allow: [api.openai.com] }
Y
out=$(run $A12S up)
check "5 only fifth created" "grep -q 'created fifth' <<<'$out' && ! grep -qE 'created .*(scout|builder|tester|reviewer)' <<<'$out'"
cp fleet.yaml.orig fleet.yaml
out=$(run $A12S up); run $A12S drive ls
check "5 fifth removed, home drive kept (orphan listed)" "grep -q 'removed fifth' <<<'$out' && $A12S drive ls | grep -q 'home-fifth'"

log "## 6 kill the SSE client mid-turn, reconnect with Last-Event-ID: no loss, no duplicates"
$A12S logs scout --follow > /tmp/gate6.a 2>&1 &
L=$!
$A12S prompt scout "Count from 1 to 60, one number per line, nothing else." > /tmp/gate6.prompt 2>&1 &
P=$!
sleep 3; kill -9 $L; wait $L 2>/dev/null
last=$(awk '{print $1}' /tmp/gate6.a | tail -1)
log "killed the logs client after 3s at seq $last; waiting for the turn to finish"
wait $P; cat /tmp/gate6.prompt >> "$LOG"
run $A12S logs scout --since "$last" > /tmp/gate6.b
cat > /tmp/gate6.py <<'PY'
import sys, json
a = [l for l in open(sys.argv[1]) if l[:1].isdigit()]
b = [l for l in open(sys.argv[2]) if l[:1].isdigit()]
rows = [(int(l.split(" ", 2)[0]), l.split(" ", 2)[1], l.split(" ", 2)[2]) for l in a + b]
seqs = [r[0] for r in rows]
dupes = len(seqs) - len(set(seqs)); gaps = [y for x, y in zip(seqs, seqs[1:]) if y != x + 1]
text = "".join(json.loads(d).get("text", "") for s, t, d in rows if t == "text.delta" and s > int(sys.argv[3]))
nums = [int(t) for t in text.split() if t.isdigit()]
ok = dupes == 0 and not gaps and nums == list(range(1, 61))
print(f"6: before kill {len(a)} events, after reconnect {len(b)}, dupes={dupes}, gaps={gaps}, numbers 1..60 complete={nums == list(range(1, 61))}")
sys.exit(0 if ok else 1)
PY
first=$(grep -E ' turn\.started ' /tmp/gate6.a /tmp/gate6.b | awk -F'[: ]' '{print $2}' | sort -n | tail -1)
python3 /tmp/gate6.py /tmp/gate6.a /tmp/gate6.b "${first:-0}" > /tmp/gate6.res; rc=$?; tee -a "$LOG" < /tmp/gate6.res
check "6 reconnect lost nothing and duplicated nothing" "[ $rc -eq 0 ]"

log "## 7 a request to a host that isn't allowed is blocked and shows up as a diagnostic"
run $A12S prompt builder "Run exactly this shell command and report its exit code and the first line of output: curl -sS -m 10 https://example.com"
run $A12S logs builder
check "7 example.com denied with a diagnostic event" "$A12S logs builder | grep diagnostic | grep -q 'example.com'"

log "## 8 bump the harness version in the lock; next turn uses it; no image rebuilt"
before=$(docker images --no-trunc --format '{{.Repository}}:{{.Tag}} {{.ID}}' | grep -E 'a12s/(slim|browser):dev' | sort)
sed -i.bak 's/version: 2.1.280, model: claude-fable-5-1/version: 2.1.281, model: claude-fable-5-1/' fleet.yaml
run $A12S up; run cat fleet.lock
out=$(run $A12S prompt scout "Reply with the single word pong.")
after=$(docker images --no-trunc --format '{{.Repository}}:{{.Tag}} {{.ID}}' | grep -E 'a12s/(slim|browser):dev' | sort)
check "8 scout's next turn runs 2.1.281" "grep -q 'claude-code 2.1.281' <<<'$out'"
check "8 tier images unchanged" "[ \"$before\" = \"$after\" ]"
cp fleet.yaml.orig fleet.yaml; rm -f fleet.yaml.bak fleet.yaml.orig

log "== leaving the fleet asleep =="
run $A12S sleep --all; run $A12S status
log "== RESULT: $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
