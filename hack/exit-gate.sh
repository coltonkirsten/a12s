#!/usr/bin/env bash
# The Layer 1 end-to-end check (see CONTRIBUTING.md), run against the local Docker engine with the examples/lab Fleet.
# Usage: hack/exit-gate.sh <logfile>. Requires: built bin/a12s and a12s/* images,
# ~/.a12s/anthropic (a setup token), ~/.a12s/lab/openai.json, and GH_TOKEN for the builder's github credential.
set -uo pipefail
LOG=${1:?logfile}; : > "$LOG"
cd "$(dirname "$0")/../examples/lab"
A12S=../../bin/a12s
PASS=0; FAIL=0
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }
run() { log "\$ $*"; "$@" 2>&1 | tee -a "$LOG"; return "${PIPESTATUS[0]}"; }
check() { local name=$1; shift; if "$@"; then PASS=$((PASS+1)); log "PASS $name"; else FAIL=$((FAIL+1)); log "FAIL $name"; fi; }
count_phase() { $A12S status | grep -cE "^(scout|builder|tester|reviewer) +$1" | grep -qx 4; }
# turn_ok FILE WORD: the send --wait command's own output must contain a started line, the word as
# streamed text (not the echoed prompt), and a completed terminal line.
turn_ok() { grep -q '^turn t_[0-9a-f]* started' "$1" && grep -vE '^(turn |\[|[0-9:]+ \$ )' "$1" | grep -q -- "$2" && grep -q '^turn t_[0-9a-f]* completed$' "$1"; }
resumed_ok() { grep -q 'resumed: true' "$1" && grep -vE '^(turn |\[|[0-9:]+ \$ )' "$1" | grep -q -- "$2"; }
grep_file() { grep -q -- "$1" "$2"; }

log "== exit gate on $(docker version --format '{{.Server.Version}} {{.Server.Arch}}') =="
cp fleet.yaml fleet.yaml.orig

log "## 1 one file: four Computers, both harnesses"
check "1 fleet declares four computers" test "$(grep -cE '^  (scout|builder|tester|reviewer):' fleet.yaml)" -eq 4

log "## 2 one command: a12s up brings all four to Ready"
$A12S creds set anthropic < ~/.a12s/anthropic && $A12S creds set openai < ~/.a12s/lab/openai.json --no-check &&
  printf %s "$GH_TOKEN" | $A12S creds set github || { log "FAIL setup: creds set"; exit 1; }
run $A12S up; run $A12S status
check "2 all four Ready" count_phase Ready

log "## 3 streamed reply for each of the four"
for c in scout builder tester reviewer; do
  $A12S send --wait $c "Reply with the single word pong." > /tmp/gate3.$c; cat /tmp/gate3.$c >> "$LOG"
  check "3 $c streams text to turn.finished" turn_ok /tmp/gate3.$c pong
done

log "## 4 sessions resume across sleep --all / wake --all"
for c in scout builder tester reviewer; do run $A12S send --wait $c "Remember the secret word: tangerine-$c. Reply only: ok."; done
run $A12S sleep --all; run $A12S status
check "4 all Asleep" count_phase Asleep
run $A12S wake --all; run $A12S status
for c in scout builder tester reviewer; do
  $A12S send --wait $c "What was the secret word I asked you to remember? Reply with just the word." > /tmp/gate4.$c; cat /tmp/gate4.$c >> "$LOG"
  check "4 $c resumed: true and recalls the word" resumed_ok /tmp/gate4.$c "tangerine-$c"
done

log "## 5 scaling is editing: add a fifth, only it is created; remove it, drives kept"
cat >> fleet.yaml <<'Y'

  fifth:
    harness: { kind: codex, version: 0.154.0 }
    image: slim
    credentials: [openai]
    egress: { allow: [api.openai.com] }
Y
$A12S up > /tmp/gate5.a; cat /tmp/gate5.a >> "$LOG"
only_fifth() { grep -q '^created fifth' /tmp/gate5.a && ! grep -qE '^(created|updated|removed) .*(scout|builder|tester|reviewer)' /tmp/gate5.a; }
check "5 only fifth created" only_fifth
cp fleet.yaml.orig fleet.yaml
$A12S up > /tmp/gate5.b; cat /tmp/gate5.b >> "$LOG"; run $A12S drive ls > /tmp/gate5.c; cat /tmp/gate5.c >> "$LOG"
fifth_removed() { grep -q '^removed fifth' /tmp/gate5.b && grep -q 'home-fifth' /tmp/gate5.c; }
check "5 fifth removed, home drive kept (orphan listed)" fifth_removed

log "## 6 kill the SSE client mid-turn, follow again: no loss, no duplicates"
$A12S logs scout --follow > /tmp/gate6.a 2>&1 &
L=$!
$A12S send --wait scout "Count from 1 to 150, one number per line, nothing else." > /tmp/gate6.prompt 2>&1 &
P=$!
head0=$($A12S logs scout 2>/dev/null | awk '{print $1}' | tail -1); head0=${head0:-0}
for i in $(seq 1 300); do awk -v h="$head0" '$1 > h && $2 == "text.delta" {f=1} END {exit !f}' /tmp/gate6.a && break; sleep 0.2; done; sleep 1
kill -9 $L; wait $L 2>/dev/null
last=$(awk '{print $1}' /tmp/gate6.a | tail -1)
log "killed the logs client mid-turn at seq $last; following again from after it"
$A12S logs scout --follow > /tmp/gate6.b 2>&1 &
L2=$!
wait $P; cat /tmp/gate6.prompt >> "$LOG"; sleep 2; kill $L2; wait $L2 2>/dev/null
cat > /tmp/gate6.py <<'PY'
import sys, json
a = [l for l in open(sys.argv[1]) if l[:1].isdigit()]
b = [l for l in open(sys.argv[2]) if l[:1].isdigit() and int(l.split(" ", 1)[0]) > int(sys.argv[3])]
rows = [(int(l.split(" ", 2)[0]), l.split(" ", 2)[1], l.split(" ", 2)[2]) for l in a + b]
started = max(s for s, t, d in rows if t == "turn.started")
mid_turn = not any(t == "turn.finished" and s > started for s, t, d in (tuple(r) for r in rows[:len(a)]))
seqs = [r[0] for r in rows]
dupes = len(seqs) - len(set(seqs)); gaps = [y for x, y in zip(seqs, seqs[1:]) if y != x + 1]
text = "".join(json.loads(d).get("text", "") for s, t, d in rows if t == "text.delta" and s > started)
nums = [int(t) for t in text.split() if t.isdigit()]
finished = any(t == "turn.finished" and s > started for s, t, d in rows)
ok = mid_turn and finished and dupes == 0 and not gaps and nums == list(range(1, 151))
print(f"6: killed mid-turn={mid_turn}; before kill {len(a)} events, after it {len(b)}; dupes={dupes}, gaps={gaps}; numbers 1..150 complete={nums == list(range(1, 151))}; turn.finished seen={finished}")
sys.exit(0 if ok else 1)
PY
python3 /tmp/gate6.py /tmp/gate6.a /tmp/gate6.b "$last" > /tmp/gate6.res 2>&1; rc=$?; tee -a "$LOG" < /tmp/gate6.res
check "6 reconnect lost nothing and duplicated nothing" test "$rc" -eq 0

log "## 7 a request to a host that isn't allowed is blocked and shows up as a diagnostic"
since7=$($A12S logs builder 2>/dev/null | awk '{print $1}' | tail -1); since7=${since7:-0}
$A12S send --wait builder "Run exactly this shell command and report its exit code and the first line of output: curl -sS -m 10 https://example.com" > /tmp/gate7.out; cat /tmp/gate7.out >> "$LOG"
$A12S logs builder | awk -v s="$since7" '$1 > s' > /tmp/gate7.log; cat /tmp/gate7.log >> "$LOG"
denied7() { grep -q '^turn t_[0-9a-f]* completed$' /tmp/gate7.out && grep ' diagnostic ' /tmp/gate7.log | grep -q '"host":"example.com"'; }
check "7 example.com denied with a diagnostic event (this turn)" denied7

log "## 8 bump the harness version; next turn uses it; no image rebuilt"
before=$(docker images --no-trunc --format '{{.Repository}}:{{.Tag}} {{.ID}}' | grep -E 'a12s/slim:dev' | sort)
sed -i.bak 's/version: 2.1.280, model: claude-fable-5-1/version: 2.1.281, model: claude-fable-5-1/' fleet.yaml
run $A12S up
$A12S send --wait scout "Reply with the single word pong." > /tmp/gate8.out; cat /tmp/gate8.out >> "$LOG"
after=$(docker images --no-trunc --format '{{.Repository}}:{{.Tag}} {{.ID}}' | grep -E 'a12s/slim:dev' | sort)
v281() { grep -q '^turn t_[0-9a-f]* started (claude-code 2.1.281' /tmp/gate8.out && turn_ok /tmp/gate8.out pong; }
check "8 scout's next turn runs 2.1.281" v281
check "8 slim image unchanged" test "$before" = "$after"
cp fleet.yaml.orig fleet.yaml; rm -f fleet.yaml.bak fleet.yaml.orig

log "== leaving the fleet asleep =="
run $A12S sleep --all; run $A12S status
log "== RESULT: $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
