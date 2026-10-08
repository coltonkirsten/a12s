# Exit gate (§8)

## Layer 1 enhancements, run 2026-10-08 on mac-mini (docs/a12s-layer1-enhancements-design.md §9)

Docker Desktop 29.2.0 (linux/arm64, 4 GB), images built from `main` after S1–S8 and the final-review fixes
(PRs #7–#16) plus this PR, Claude Code 2.1.280 bundle on all three Computers (`claude-haiku-4-5`), the lab
setup-token path (`~/.a12s/anthropic`, the Mac's OAuth access token, deleted after the run), reference Services
from `make services`. Fleet: `examples/services/fleet.yaml` renamed `gate` (§3's Fleet with local image tags),
started from nothing with `A12S_TEST_TIME_SCALE=60` (scout's 15-minute idle is 15 s). Script:
`hack/exit-gate-enhancements.sh <workdir> <log> setup inbox services amendment cli teardown`, 04:12:41–04:21:45 UTC:
**54 checks, 54 passed**, then `down --purge`. An earlier full run passed 52/54; the two misses were the gate's own
busy Turn (Claude Code refuses a bare `sleep`), fixed by waiting in python. Raw log (281 lines, no secrets):
`~/raven-v2/deliverables/a12s-build/gate/gate.log`.

Covered here: §9 items 1, 2, 3, 4, 6, 8 (the 429), 9, 10, 11, 12 (the `a12s-db` tool answered through the stdio
bridge), 14, 15, 16, 17, 18, 19 (scaffold, first-run fix line, `--json` on every listing), 20, 21, 22 (limit
sources) and 23 (auto-sleep, manual-sleep refusal, `--allow-wake` wake by `prompt`). Covered by the Go/computerd
test suites instead of this run: 5 (Controller crash, `down` with the Controller dead), 7 (the 2,000-message race),
8's byte-ceiling boundaries, and the hook-driven races of 20–23. Item 13's mechanics (memory-only login container,
0600 host write, Ready with the credential in tmpfs only) were run on this Docker during S6 with a stand-in login
command; completing GitHub's device flow needs a person at a browser and was not run.

```text
PASS setup scout Ready
PASS setup builder Ready
PASS setup reviewer Ready
PASS setup service clock Ready
PASS setup service pg Ready
PASS setup service db Ready
PASS setup service hooks Ready
PASS 1 one Turn carries the three sends in seq order, rendered per 4.8
PASS 9 queued, then streamed to completion
PASS 2 stays Asleep with one pending
PASS 2 delivered after wake
PASS 3 two inbox.discarded with bodies, no Turn
PASS 4 returned and delivered next Turn with attempts 1
PASS 4 after dispatch: delivered once, Turn interrupted
PASS 6 [a] then [b,c] in a new session
PASS 8 51st refused inbox_full with a fix
PASS 10 inbox holds resume_failed, nothing runs
PASS 10 resume --new delivers in a new session
PASS 11 at least two distinct messages via service:clock
PASS 11 send to reviewer refused 403 and audited
PASS 12 the a12s-db tool ran through the bridge
PASS 12 scout can't read the secret or resolve .a12s; CONNECT pg.a12s is denied
PASS 12 reviewer (no uses) denied with a diagnostic
PASS 14 asleep db answers 503 through the proxy, diagnostic journaled
PASS 14 wake db -> Ready
PASS 14 removed pg: stopped, pg-state an orphan, nothing destroyed
PASS 15 new ref appended; old pin kept
PASS 15 lock update keeps the key, moves explicitly
PASS 15 doctor warns on npx
PASS 16 reviewer got a message via computer:builder
PASS 16 send to scout refused with the fix, journaled
PASS 17 signed post enqueued, unsigned rejected
PASS 17 status prints the publish warning
PASS 18 owner API unreachable, no Docker socket
PASS 18 PUT /v1/fleet and POST /v1/leases are 403 over mTLS; the granted send works
PASS 18 after sleep the copied cert is refused
PASS 20 expires 0 while busy: exit 1, not_idle busy
PASS 20 a 5 s window expires while busy
PASS 20 expires 0 to a sleeping scout: asleep, no wake
PASS 21 one survivor, superseded 7, seven replaced, at the tail after a
PASS 22 limits carry their source
PASS 23 scout auto-sleeps after its idle time
PASS 23 prompt to a manually slept scout fails with the reason
PASS 23 with --allow-wake, local wakes it: prompt prints waking and streams
PASS 19 init --with db scaffolds a valid Fleet
PASS 19 the first up stops with the fix line
PASS 19 --json parses: a12s status
PASS 19 --json parses: a12s service ls
PASS 19 --json parses: a12s service status db
PASS 19 --json parses: a12s inbox builder
PASS 19 --json parses: a12s drive ls
PASS 19 --json parses: a12s token ls
PASS 19 --json parses: a12s doctor
PASS 19 logs --json is JSON lines
```

## Re-run 2026-10-06 on mac-mini (Layer 1 closeout)

Docker Desktop 29.2.0 (linux/arm64, 4 GB), images rebuilt from the closeout code at `7bb5dad` (PR #3: #58, #59, #35,
the round-1/2 closeout fixes #60–#65, and the setup-token lab example), Claude Code 2.1.280/2.1.281 and Codex 0.154.0
HarnessBundles. `examples/lab` bindings as committed: `anthropic` is a raw `sk-ant-oat…` token in `~/.a12s/anthropic`,
wired as `CLAUDE_CODE_OAUTH_TOKEN` (for this run, the Mac's current OAuth access token; a `claude setup-token` token
takes the same path), `openai` the Codex login copy in `~/.a12s/lab/openai.json`, `github` from `GH_TOKEN`.
Started from nothing (`a12s down --purge --yes`, no `fleet.lock`), `hack/exit-gate.sh` 00:51:01–00:52:12 UTC
(2026-10-07 UTC, 2026-10-06 Pacific): **17 checks, 17 passed, all 8 items pass**. Raw log:
`docs/exit-gate-2026-10-06.log` (344 lines, no secrets). The earlier commits of the branch also passed 17/17 (00:10
and 00:44 UTC). Torn down afterwards with `a12s down --purge --yes`.

## First pass — run 2026-10-05 on mac-mini

Docker Desktop 29.2.0 (linux/arm64), real Claude Code 2.1.280/2.1.281 and Codex 0.154.0 HarnessBundles,
Colton's subscription credentials injected from `~/.a12s/lab/` through `a12s.bindings.yaml`.
Script: `hack/exit-gate.sh`. Raw log: `docs/exit-gate-2026-10-05.log` (459 lines, no secrets; not committed, `*.log` is git-ignored).
Result: **17 checks, 17 passed, all 8 items pass** (run 22:13–22:14 UTC on the signed-off tree; earlier runs at 20:11, 20:57, 21:13, 21:23 and 21:46 also passed 17/17). The Fleet was started from nothing and left asleep.

| # | Item | Evidence in the log |
|---|---|---|
| 1 | One file declares four Computers, both harnesses, slim and browser | `examples/lab/fleet.yaml` is §3 verbatim: scout (claude-code, slim), builder (codex, slim), tester (claude-code, browser), reviewer (codex, slim) |
| 2 | `a12s up` brings all four to Ready | 22:13:19 `a12s up` → `started controller`, `pinned in fleet.lock: browser, slim, claude-code@2.1.280, codex@0.154.0, ./presets/scout`, `created builder, reviewer, scout, tester`; 22:13:27 status shows all four `Ready` |
| 3 | `a12s prompt <any>` streams `text.delta` to `turn.finished` for each of the four | four `turn t_… started (…)` lines, `pong`, `turn … completed` for scout, builder, tester (browser tier) and reviewer |
| 4 | Sessions resume: tell each a word, `sleep --all`, `wake --all`, ask for it back | all four `Asleep` at 22:13:57; after wake each answers `tangerine-<name>` with `resumed: true` in the started line |
| 5 | Scaling is editing: add a fifth, only it is created; remove it, drives kept | `created fifth` alone; after removing it `removed fifth` and `a12s drive ls` lists `home-fifth`; status warns `orphaned drive home-fifth` |
| 6 | Kill the SSE client mid-turn, reconnect with `Last-Event-ID`: nothing lost or duplicated | `a12s logs scout --follow` killed with SIGKILL mid-turn (after this turn's first `text.delta`) at seq 60 while scout counted 1..150; reconnect `--follow --since 60` delivered the remaining 22 events: `dupes=0, gaps=[], numbers 1..150 complete=True, turn.finished seen=True` |
| 7 | A request to a host that isn't allowed is blocked and shows as a `diagnostic` | builder (allow: api.openai.com, github.com, registry.npmjs.org) ran `curl https://example.com` and reported `curl: (56) CONNECT tunnel failed, response 403`; that turn's journal: `diagnostic {"message":"egress denied","data":{"host":"example.com","port":443,"reason":"host is not in the egress allowlist"},"source":"controller"}` |
| 8 | Bump the harness version in the lock; the next turn uses it; no image rebuilt | scout 2.1.280 → 2.1.281 in the Fleet, `a12s up` → `pinned in fleet.lock: claude-code@2.1.281`, `updated scout`; next turn: `turn … started (claude-code 2.1.281, claude-fable-5-1, resumed: true)`; `a12s/slim:dev` and `a12s/browser:dev` image IDs unchanged |

Notes
- Every Claude turn also journals denied egress to `mcp-proxy.anthropic.com` and `http-intake.logs.us5.datadoghq.com`, and Codex to `*.oaiusercontent.com`: non-essential traffic the allowlist blocks, visible as the spec intends.
- `a12s up` never wakes a sleeping Computer (power is an operation, not a definition); a gate run therefore starts from a torn-down Fleet.
- Teardown for a re-run is `docker rm/network rm/volume rm` by label `a12s.fleet=lab` plus `~/.a12s/lab/controller.json`; there was deliberately no `a12s down` at the time (`a12s down --purge` does this since 2026-10-06).
