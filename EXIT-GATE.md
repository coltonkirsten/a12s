# Exit gate (§8)

## Re-run 2026-10-06 on mac-mini (Layer 1 closeout)

Docker Desktop 29.2.0 (linux/arm64, 4 GB), images rebuilt from the closeout tree (PR #3: #58, #59, #35 and the
setup-token lab example), Claude Code 2.1.280/2.1.281 and Codex 0.154.0 HarnessBundles. `examples/lab` bindings as
committed: `anthropic` is a raw `sk-ant-oat…` token in `~/.a12s/anthropic`, wired as `CLAUDE_CODE_OAUTH_TOKEN` (for
this run, the Mac's current OAuth access token; a `claude setup-token` token takes the same path), `openai` the Codex
login copy in `~/.a12s/lab/openai.json`, `github` from `GH_TOKEN`.
Started from nothing (`a12s down --purge --yes`), `hack/exit-gate.sh` 00:10:19–00:11:49 UTC:
**17 checks, 17 passed, all 8 items pass**. Raw log: `docs/exit-gate-2026-10-06.log` (344 lines, no secrets).
Torn down afterwards with `a12s down --purge --yes`.

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
