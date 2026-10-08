#!/usr/bin/env bash
# The enhancements exit gate (docs/a12s-layer1-enhancements-design.md §9), against the local Docker
# engine with examples/services (renamed fleet "gate"). Usage:
#   hack/exit-gate-enhancements.sh <workdir> <logfile> [sections...]
# Needs: bin/a12s, a12s/{controller,slim}:dev, the claude-code 2.1.280 bundle, `make services`, and
# ~/.a12s/anthropic (a setup-token or OAuth access token). Sections: setup inbox services amendment teardown.
set -uo pipefail
W=${1:?workdir}; LOG=${2:?logfile}; shift 2
SECTIONS=${*:-setup inbox services amendment}
REPO=$(cd "$(dirname "$0")/.." && pwd)
A12S="$REPO/bin/a12s"
mkdir -p "$W"; cd "$W"
PASS=0; FAIL=0
log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }
run() { log "\$ ${*/#a /a12s }"; "$@" 2>&1 | tee -a "$LOG"; return "${PIPESTATUS[0]}"; }
check() { local name=$1; shift; if "$@" >>"$LOG" 2>&1; then PASS=$((PASS+1)); log "PASS $name"; else FAIL=$((FAIL+1)); log "FAIL $name"; fi; }
a() { "$A12S" "$@"; }
phase() { a status --json | python3 -c "import json,sys; d=json.load(sys.stdin); print(next((c['phase'] for c in d['computers']+d.get('services',[]) if c['name']=='$1'), ''))"; }
wait_phase() { for _ in $(seq 1 ${3:-120}); do [ "$(phase "$1")" = "$2" ] && return 0; sleep 1; done; log "timeout: $1 is $(phase "$1"), want $2"; return 1; }
events() { a logs "$1" --json 2>/dev/null; }
inbox_json() { a inbox "$1" --all --json; }
msg_state() { a inbox "$1" --all --json | python3 -c "import json,sys; d=json.load(sys.stdin); print(next((m['state'] for m in d['messages'] if m['id']=='$2'), 'none'))"; }
send_id() { a send --json "$@" | python3 -c "import json,sys; print(json.load(sys.stdin)['id'])"; }
idle() { for _ in $(seq 1 ${2:-180}); do a status --json | python3 -c "
import json,sys; d=json.load(sys.stdin); c=next(c for c in d['computers'] if c['name']=='$1'); sys.exit(0 if c['phase']=='Ready' and not c.get('activeTurn') else 1)" && return 0; sleep 1; done; return 1; }
long_turn() { a prompt "$1" "Write the numbers from 1 to 400, one per line, with no other text." > /tmp/gate.long.$1 2>&1 & sleep 6; }

if [[ " $SECTIONS " == *" setup "* ]]; then
  log "== enhancements exit gate on $(docker version --format '{{.Server.Version}} {{.Server.Arch}}'), a12s $(git -C "$REPO" rev-parse --short HEAD) =="
  sed -e 's/metadata: { name: lab }/metadata: { name: gate }/' "$REPO/examples/services/fleet.yaml" > fleet.yaml
  # Gate 11 adds the minute tick later; the base Fleet keeps the clock quiet.
  python3 - <<'PY'
import re
s=open('fleet.yaml').read()
s=s.replace('        - { name: tick, cron: "* * * * *", to: scout, body: "Clock tick: reply with the single word tock." }\n','')
s=s.replace('        - { name: not-granted, cron: "* * * * *", to: reviewer, body: "This job has no grant and is refused." }\n','')
open('fleet.yaml','w').write(s)
PY
  mkdir -p ~/.a12s/gate
  umask 077; [ -s ~/.a12s/gate/postgres ] || head -c 18 /dev/urandom | base64 | tr -d '/+=' > ~/.a12s/gate/postgres
  [ -s ~/.a12s/gate/gh-webhook ] || head -c 18 /dev/urandom | base64 | tr -d '/+=' > ~/.a12s/gate/gh-webhook
  cat > a12s.bindings.yaml <<'B'
credentials:
  anthropic:  { file: ~/.a12s/anthropic }
  postgres:   { file: ~/.a12s/gate/postgres }
  gh-webhook: { file: ~/.a12s/gate/gh-webhook }
B
  rm -f fleet.lock
  A12S_TEST_TIME_SCALE=60 run a up || { log "FAIL setup: a12s up"; exit 1; }
  run a status
  for s in scout builder reviewer; do check "setup $s Ready" wait_phase $s Ready 180; done
  for s in clock pg db hooks; do check "setup service $s Ready" wait_phase $s Ready 180; done
fi

if [[ " $SECTIONS " == *" inbox "* ]]; then
  log "## 1 batched while busy"
  idle builder; long_turn builder
  for who in alice bob carol; do send_id builder "note from $who: reply OK" --from "$who" > /tmp/gate1.$who; done
  idle builder 300; sleep 3; idle builder 300
  events builder > /tmp/gate1.ev
  batch3() { python3 - /tmp/gate1.alice /tmp/gate1.bob /tmp/gate1.carol < /tmp/gate1.ev <<'PY'
import json,sys
want=[open(f).read().strip() for f in sys.argv[1:]]
for l in sys.stdin:
    e=json.loads(l)
    if e['type']!='turn.started': continue
    ib=(e.get('data') or {}).get('inbox') or {}
    got=[m['id'] for m in ib.get('messages',[])]
    if got==want:
        p=e['data'].get('prompt','')
        ok='[a12s · how to read these messages]' in p and p.count('[a12s · how to read these messages]')==1 and 'from="alice" via="local"' in p
        sys.exit(0 if ok else 2)
sys.exit(1)
PY
  }
  check "1 one Turn carries the three sends in seq order, rendered per 4.8" batch3

  log "## 9 prompt keeps its UX: busy prints queued, then streams"
  long_turn builder
  a prompt builder "Reply with the single word pong." > /tmp/gate9 2>&1; cat /tmp/gate9 >> "$LOG"
  check "9 queued, then streamed to completion" sh -c "grep -q '^queued: builder' /tmp/gate9 && grep -q 'pong' /tmp/gate9 && grep -q 'completed' /tmp/gate9"
  idle builder 300

  log "## 2 no wake without a policy (reviewer has none)"
  run a sleep reviewer; wait_phase reviewer Asleep
  id2=$(send_id reviewer "Reply with the single word kiwi.")
  sleep 5
  check "2 stays Asleep with one pending" sh -c "[ \"\$($A12S status --json | python3 -c \"import json,sys; d=json.load(sys.stdin); c=next(c for c in d['computers'] if c['name']=='reviewer'); print(c['phase'], c['inbox']['pending'])\")\" = 'Asleep 1' ]"
  run a wake reviewer; wait_phase reviewer Ready; sleep 3; idle reviewer 180
  check "2 delivered after wake" sh -c "[ \"\$(msg_state reviewer $id2 2>/dev/null || true)\" = delivered ] || $A12S inbox reviewer --all --json | grep -q '\"state\": \"delivered\"'"

  log "## 3 clear on wake"
  run a sleep reviewer; wait_phase reviewer Asleep
  c1=$(send_id reviewer "discard me one"); c2=$(send_id reviewer "discard me two")
  run a wake reviewer --clear-inbox; wait_phase reviewer Ready; sleep 5
  events reviewer > /tmp/gate3.ev
  check "3 two inbox.discarded with bodies, no Turn" sh -c "[ \$(grep -c '\"type\":\"inbox.discarded\"' /tmp/gate3.ev) -ge 2 ] && grep -q 'discard me one' /tmp/gate3.ev && grep -q 'discard me two' /tmp/gate3.ev && ! grep 'turn.started' /tmp/gate3.ev | grep -q '$c1'"

  log "## 4 redelivery before dispatch, not after"
  docker exec -u 0 a12s-gate-reviewer sh -c 'mkdir -p /home/.a12s/test-hooks && touch /home/.a12s/test-hooks/exit-after-accept'
  id4=$(send_id reviewer "Reply with the single word plum.")
  sleep 3; wait_phase reviewer Ready 180; idle reviewer 240
  attempts() { a inbox reviewer --all --json | python3 -c "import json,sys; d=json.load(sys.stdin); m=next(m for m in d['messages'] if m['id']=='$id4'); print(m['state'], m['attempts'])"; }
  check "4 returned and delivered next Turn with attempts 1" sh -c "[ \"\$($(declare -f attempts); A12S=$A12S; a(){ \$A12S \"\$@\"; }; id4=$id4; attempts)\" = 'delivered 1' ]"
  docker exec -u 0 a12s-gate-reviewer sh -c 'touch /home/.a12s/test-hooks/exit-after-dispatch'
  id4b=$(send_id reviewer "Reply with the single word fig.")
  sleep 3; wait_phase reviewer Ready 180; idle reviewer 240
  events reviewer > /tmp/gate4.ev
  check "4 after dispatch: delivered once, Turn interrupted" sh -c "[ \"\$(msg_state reviewer $id4b)\" = delivered ] && grep -q 'computerd restarted' /tmp/gate4.ev"

  log "## 6 fresh split"
  long_turn builder
  fa=$(send_id builder "split a: reply A"); fb=$(send_id builder "split b: reply B" --fresh); fc=$(send_id builder "split c: reply C")
  idle builder 300; sleep 5; idle builder 300
  events builder > /tmp/gate6.ev
  split() { python3 - "$fa" "$fb" "$fc" < /tmp/gate6.ev <<'PY'
import json,sys
a,b,c=sys.argv[1:]
batches=[]
for l in sys.stdin:
    e=json.loads(l)
    if e['type']=='turn.started' and (e.get('data') or {}).get('inbox'):
        batches.append([m['id'] for m in e['data']['inbox']['messages']])
ok=[a] in batches and [b,c] in batches
sys.exit(0 if ok else 1)
PY
  }
  check "6 [a] then [b,c] in a new session" sh -c "$(declare -f split); fa=$fa fb=$fb fc=$fc split && grep -q session.changed /tmp/gate6.ev"

  log "## 8 limits: builder takes 50 outstanding, the 51st is 429"
  run a sleep builder; wait_phase builder Asleep
  for i in $(seq 1 50); do a send builder "limit filler $i" >/dev/null || break; done
  a send builder "the 51st" > /tmp/gate8 2>&1; cat /tmp/gate8 >> "$LOG"
  check "8 51st refused inbox_full with a fix" sh -c "grep -q inbox_full /tmp/gate8 && grep -q 'fix:' /tmp/gate8"
  run a inbox clear builder --yes
  run a wake builder; wait_phase builder Ready

  log "## 10 resume failure holds"
  idle builder
  docker exec -u 0 a12s-gate-builder sh -c 'echo "{\"sessionId\":\"s_damaged\",\"harness\":\"claude-code\",\"cwd\":\"/home/agent\"}" > /home/.a12s/session.json'
  id10=$(send_id builder "Reply with the single word lime.")
  sleep 15
  check "10 inbox holds resume_failed, nothing runs" sh -c "$A12S inbox builder --json | grep -q '\"holdReason\": \"resume_failed\"'"
  run a inbox resume builder --new; sleep 5; idle builder 300
  check "10 resume --new delivers in a new session" sh -c "[ \"\$(msg_state builder $id10)\" = delivered ]"
fi

if [[ " $SECTIONS " == *" services "* ]]; then
  log "## 11 clock: two minute ticks to scout via service:clock; a job without a grant is 403 and audited"
  python3 - <<'PY'
s=open('fleet.yaml').read()
s=s.replace('      jobs:\n','      jobs:\n        - { name: tick, cron: "* * * * *", to: scout, body: "Clock tick: reply with the single word tock." }\n        - { name: not-granted, cron: "* * * * *", to: reviewer, body: "refused" }\n',1)
open('fleet.yaml','w').write(s)
PY
  run a up; wait_phase clock Ready 120
  sleep 150
  ticks() { a inbox scout --all --json | python3 -c "import json,sys; d=json.load(sys.stdin); m=[x for x in d['messages'] if x['via']=='service:clock']; print(len(m), len(set(x['id'] for x in m)))"; }
  check "11 at least two messages via service:clock" sh -c "n=\$($(declare -f ticks); A12S=$A12S; a(){ \$A12S \"\$@\"; }; ticks | cut -d' ' -f1); [ \"\$n\" -ge 2 ]"
  a service logs clock > /tmp/gate11 2>&1; cat /tmp/gate11 | tail -20 >> "$LOG"
  check "11 send to reviewer refused 403 and audited" sh -c "grep -q 'not-granted to reviewer refused: 403 no_grant' /tmp/gate11 && grep -q 'denied: send to reviewer (no grant)' /tmp/gate11"
  python3 - <<'PY'
s=open('fleet.yaml').read()
s=s.replace('        - { name: tick, cron: "* * * * *", to: scout, body: "Clock tick: reply with the single word tock." }\n        - { name: not-granted, cron: "* * * * *", to: reviewer, body: "refused" }\n','')
open('fleet.yaml','w').write(s)
PY
  run a up

  log "## 12 capability without the secret"
  wait_phase db Ready 120
  a prompt scout "Use the a12s-db tool to list the tables in the database, then reply with the word DONE and how many tables there are." > /tmp/gate12 2>&1; cat /tmp/gate12 >> "$LOG"
  check "12 the a12s-db tool ran through the bridge" sh -c "grep -q 'a12s-db' /tmp/gate12 && grep -q 'completed' /tmp/gate12"
  a shell scout -- sh -c 'cat /run/a12s/secrets/postgres 2>&1; getent hosts db.a12s || echo NODNS-db; getent hosts pg.a12s || echo NODNS-pg; curl -s -m5 -x http://a12s-gate-controller:3128 https://pg.a12s:5432 -o /dev/null -w "pg:%{http_code}\n"' > /tmp/gate12s 2>&1; cat /tmp/gate12s >> "$LOG"
  check "12 scout can't read the secret or resolve .a12s; CONNECT pg.a12s is denied" sh -c "! grep -q \"\$(cat ~/.a12s/gate/postgres)\" /tmp/gate12s && grep -q NODNS-db /tmp/gate12s && grep -q NODNS-pg /tmp/gate12s && grep -q 'pg:000\|pg:403' /tmp/gate12s"
  a shell reviewer -- sh -c 'curl -s -m5 -x http://a12s-gate-controller:3128 http://db.a12s:8080/healthz; echo' > /tmp/gate12r 2>&1; cat /tmp/gate12r >> "$LOG"
  check "12 reviewer (no uses) denied with a diagnostic" sh -c "grep -q 'reviewer cannot reach db' /tmp/gate12r && $A12S logs reviewer | grep -q 'reviewer cannot reach db'"

  log "## 14 lifecycle"
  run a sleep db; wait_phase db Asleep
  a shell scout -- sh -c 'curl -s -m5 -o /dev/null -w "%{http_code}" -x http://a12s-gate-controller:3128 http://db.a12s:8080/healthz' > /tmp/gate14 2>&1
  check "14 asleep db answers 503 through the proxy, diagnostic journaled" sh -c "grep -q 503 /tmp/gate14 && $A12S logs scout | grep -q 'db is Asleep'"
  run a wake db; check "14 wake db -> Ready" wait_phase db Ready 120
  cp fleet.yaml fleet.yaml.full
  python3 - <<'PY'
s=open('fleet.yaml').read()
i=s.index('  pg:\n'); j=s.index('  db:\n')
s=s[:i]+s[j:]
s=s.replace('    uses: [pg]\n','').replace('      hostPortFrom: { A12S_SERVICE_PG_DB: [PGHOST, PGPORT] }\n','')
open('fleet.yaml','w').write(s)
PY
  run a up; run a drive ls > /tmp/gate14d; cat /tmp/gate14d >> "$LOG"
  check "14 removed pg: stopped, pg-state an orphan, nothing destroyed" sh -c "grep -q 'pg-state.*ORPHANED' /tmp/gate14d && ! docker ps --format '{{.Names}}' | grep -qx a12s-gate-pg"
  cp fleet.yaml.full fleet.yaml; run a up; wait_phase pg Ready 120

  log "## 15 pinning"
  docker tag a12s/svc-cron:0.1 a12s/svc-cron:0.1-gate
  sed -i '' 's|image: a12s/svc-cron:0.1$|image: a12s/svc-cron:0.1-gate|' fleet.yaml
  run a up
  check "15 new ref appended; old pin kept" sh -c "grep -q 'clock@a12s/svc-cron:0.1-gate' fleet.lock && grep -q 'clock@a12s/svc-cron:0.1:' fleet.lock"
  run a lock update services/clock
  check "15 lock update keeps the key, moves explicitly" grep -q 'clock@a12s/svc-cron:0.1-gate' fleet.lock
  sed -i '' 's|image: a12s/svc-cron:0.1-gate$|image: a12s/svc-cron:0.1|' fleet.yaml; run a up
  cp fleet.yaml fleet.npx.yaml
  sed -i '' 's|command: \["/opt/mcp/bin/mcp-server-postgres"\]|command: ["npx", "-y", "@modelcontextprotocol/server-postgres"]|' fleet.npx.yaml
  a --fleet fleet.npx.yaml doctor > /tmp/gate15d 2>&1; cat /tmp/gate15d >> "$LOG"
  check "15 doctor warns on npx" grep -q 'runs npx at start' /tmp/gate15d

  log "## 16 agent-to-agent"
  a prompt builder "Use your a12s send tool to send the message 'please review PR 42' to reviewer. Then try sending 'hi' to scout and tell me exactly what error came back." > /tmp/gate16 2>&1; cat /tmp/gate16 >> "$LOG"
  check "16 reviewer got a message via computer:builder" sh -c "$A12S inbox reviewer --all --json | grep -q '\"via\": \"computer:builder\"'"
  check "16 send to scout refused with the fix, journaled" sh -c "grep -q 'add scout to computers.builder.grants.send' /tmp/gate16 && $A12S logs builder | grep -q 'builder may not send to scout'"

  log "## 17 webhook"
  body='{"action":"opened","repository":{"full_name":"c/a12s"},"pull_request":{"title":"Gate","html_url":"https://x/1"}}'
  sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$(cat ~/.a12s/gate/gh-webhook)" | sed 's/^.*= //')
  code1=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:8787/github -H "X-GitHub-Event: pull_request" -H "X-Hub-Signature-256: sha256=$sig" -d "$body")
  code2=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:8787/github -H "X-GitHub-Event: pull_request" -d "$body")
  log "signed: $code1, unsigned: $code2"
  check "17 signed post enqueued, unsigned rejected" sh -c "[ $code1 = 202 ] && [ $code2 = 401 ] && $A12S inbox builder --all --json | grep -q 'webhook:github'"
  check "17 status prints the publish warning" sh -c "$A12S status | grep -q 'reachable outside uses'"

  log "## 18 no escalation from inside a Service"
  a shell clock -- sh -c 'wget -q -T 3 -O- http://a12s-gate-controller:7000/v1/status 2>&1 | head -1; ls /var/run/docker.sock 2>&1' > /tmp/gate18 2>&1; cat /tmp/gate18 >> "$LOG"
  check "18 owner API unreachable, no Docker socket" sh -c "! grep -q fleet /tmp/gate18 && grep -q 'No such file' /tmp/gate18"
  rm -rf /tmp/gate-cert; mkdir -p /tmp/gate-cert
  for f in ca.crt cert.pem key.pem; do docker exec -u 0 a12s-gate-clock cat /run/a12s/tls/$f > /tmp/gate-cert/$f; done
  docker cp /tmp/gate-cert a12s-gate-scout:/tmp/cert >/dev/null
  mt() { a shell scout --root -- sh -c "curl -s -o /dev/null -w '%{http_code}' --cacert /tmp/cert/ca.crt --cert /tmp/cert/cert.pem --key /tmp/cert/key.pem -X $1 https://a12s-gate-controller:7443$2 -H 'content-type: application/json' -d '$3'"; }
  r1=$(mt PUT /v1/fleet '{}'); r2=$(mt POST /v1/leases '{}'); r3=$(mt POST /v1/computers/scout/inbox '{"body":"from a copied clock key"}')
  log "with the clock's copied key: PUT /v1/fleet $r1, POST /v1/leases $r2, send to scout $r3"
  check "18 PUT /v1/fleet and POST /v1/leases are 403 over mTLS" sh -c "[ $r1 = 403 ] && [ $r2 = 403 ]"
  run a sleep clock; wait_phase clock Asleep
  r4=$(mt POST /v1/computers/scout/inbox '{"body":"after sleep"}')
  log "after a12s sleep clock: $r4"
  check "18 after sleep the copied cert is refused" sh -c "[ $r4 = 403 ]"
  rm -rf /tmp/gate-cert; a shell scout --root -- rm -rf /tmp/cert
  run a wake clock; wait_phase clock Ready 120
fi

if [[ " $SECTIONS " == *" amendment "* ]]; then
  log "## 20 expiry"
  long_turn builder
  a send builder "now or never" --expires 0 > /tmp/gate20a 2>&1; e1=$?; cat /tmp/gate20a >> "$LOG"
  check "20 expires 0 while busy: exit 1, not_idle busy" sh -c "[ $e1 = 1 ] && grep -q busy /tmp/gate20a"
  id20=$(send_id builder "five-second window" --expires 5s)
  sleep 8
  check "20 a 5 s window expires while busy" sh -c "[ \"\$(msg_state builder $id20)\" = expired ]"
  idle builder 300
  run a sleep scout; wait_phase scout Asleep
  a send scout "now or never" --expires 0 > /tmp/gate20b 2>&1; e2=$?; cat /tmp/gate20b >> "$LOG"; sleep 3
  check "20 expires 0 to a sleeping scout: asleep, no wake" sh -c "[ $e2 = 1 ] && grep -q asleep /tmp/gate20b && [ \"\$(phase scout)\" = Asleep ]"

  log "## 21 replace"
  long_turn builder
  r0=$(send_id builder "deploy-check 0" --replace deploy-check)
  la=$(send_id builder "a, between the first and second")
  for i in 1 2 3 4 5 6 7; do rl=$(send_id builder "deploy-check $i" --replace deploy-check); done
  rep() { a inbox builder --all --json | python3 -c "
import json,sys; d=json.load(sys.stdin); ms=d['messages']
pend=[m for m in ms if m['state']=='pending' and m.get('replaceKey')=='deploy-check']
repl=[m for m in ms if m['state']=='replaced' and m.get('replaceKey')=='deploy-check']
a=[m for m in ms if m['id']=='$la'][0]
print(len(pend), pend[0]['superseded'] if pend else -1, len(repl), pend[0]['seq']>a['seq'] if pend else False)"; }
  check "21 one pending, superseded 7, seven replaced, at the tail" sh -c "[ \"\$($(declare -f rep); A12S=$A12S; a(){ \$A12S \"\$@\"; }; la=$la; rep)\" = '1 7 7 True' ]"
  idle builder 300

  log "## 22 queue size: builder overrides maxMessages to 50 (defaults 200)"
  check "22 limits carry their source" sh -c "$A12S inbox builder --json | python3 -c \"import json,sys; d=json.load(sys.stdin); l=d['limits']; sys.exit(0 if l['maxMessages']['value']==50 and l['maxMessages']['source']=='computer' else 1)\""

  log "## 23 power policy (scout: sleepAfterIdle 15m, at test speed 60x = 15 s)"
  run a wake scout; wait_phase scout Ready; idle scout
  check "23 scout auto-sleeps after its idle time" wait_phase scout Asleep 90
  a sleep scout >/dev/null; wait_phase scout Asleep
  a prompt scout "hi" > /tmp/gate23a 2>&1
  check "23 prompt to a manually slept scout fails with the reason" sh -c "grep -q 'asleep' /tmp/gate23a"
  a sleep scout --allow-wake >/dev/null
  a prompt scout "Reply with the single word wren." > /tmp/gate23b 2>&1; cat /tmp/gate23b >> "$LOG"
  check "23 with --allow-wake, local wakes it: prompt prints waking and streams" sh -c "grep -q 'waking scout' /tmp/gate23b && grep -qi wren /tmp/gate23b"
fi

if [[ " $SECTIONS " == *" teardown "* ]]; then
  run a down --purge --yes
  rm -rf ~/.a12s/gate
  docker rmi a12s/svc-cron:0.1-gate >/dev/null 2>&1 || true
fi
log "== $PASS passed, $FAIL failed =="
