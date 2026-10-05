# Reviews

Each round: Astra (GPT-6 Astra, `codex exec -m gpt-6-astra`, reasoning xhigh, read-only sandbox)
reviews the tree against `docs/a12s-layer1-design.md`, `docs/adapter-check-2026-10-05.md` and `PLAN.md`
for correctness, security, simplicity and anything overbuilt. Fable 5.1 (the integrator) triages, the
Opus implementers fix. Raw reports are kept outside the repo (`~/raven-v2/deliverables/647307f25766/astra_r*.md`).

## Round 1 — M1–M3 at 88fd791 (fleet, driver, docker, api, computerd, images, CI)

Verdict: not signed off. 23 findings: 9 blockers, 13 should-fix, 1 nit.

| # | Finding (Astra) | Decision | What changed |
|---|---|---|---|
| 1 | Prompt dispatched before durable acceptance (`requestId` could run twice after a crash) | accept | acceptance journaled before the harness is called |
| 2 | Unreadable `session.json` silently becomes "no session" → fresh start | accept | record validated; damaged record → `resume.failed`, `session: new` or repair required |
| 3 | Claude resume accepts a replacement session id (`session.changed` overwrites) | accept | mismatch → interrupt, `resume.failed`, old id kept |
| 4 | Turns left without a terminal event after a computerd crash | accept | startup journals `turn.finished interrupted` for unfinished turns |
| 5 | Stop can't reach a turn still starting; Codex control requests have no deadline | accept | cancellable start, 60s deadline on app-server requests |
| 6 | Credential generation acknowledged before the long-lived Codex child restarts | accept | generation reported only after wiring + restart |
| 7 | Codex ignores a model change after wake | accept | model sent on every `turn/start` |
| 8 | `strictMcpConfig: true` contradicts PLAN | accept (already fixed in b85b899) | — |
| 9 | Fresh work drives are root-owned, agent cannot write | accept | driver passes `A12S_DRIVES`; computerd chowns empty rw mount roots |
| 10 | Bundle cache keyed by kind/version only: a different image reuses old files | accept | cache dir keyed `<kind>-<version>-<imageID[:12]>` |
| 11 | Go event payload shapes differ from the wire (`harnessVersion`, `session`) | accept | Go structs aligned with computerd; fixtures from the TS source decoded in tests |
| 12 | Root chown/mkdir through agent-writable parents → journal takeover via symlink | accept | agent-dir setup runs as uid 1000 / O_NOFOLLOW |
| 13 | Agent-owned `secrets/` lets the agent point a credential name at the TLS key | accept | `secrets/` root 0711, only declared names read, symlinks rejected |
| 14 | Refreshed subscription tokens left on the home drive / lost | accept (already fixed in bd9fdf8) | reclaim-to-tmpfs after every turn and at shutdown |
| 15 | Browser launched without the sandbox config | accept (already fixed in b85b899) | documented `--config` invocation used |
| 16 | `NODE_PATH` dropped on the browser tier | accept | set on the browser tier |
| 17 | Existing networks reused without checking isolated gateway mode | accept | reuse requires internal + isolated + labels |
| 18 | `--init` missing | accept | `HostConfig.Init` set |
| 19 | SSE ignores backpressure | accept | paged replay, drain-aware live delivery |
| 20 | No generation fence on `removeComputer` | **reject** | the reconciler is serialized per Fleet, so a stale removal cannot race a newer generation; §4 defines `removeComputer(id)` and widening it buys nothing today |
| 21 | Failed preset copy reported as success on retry | accept | container removed on failure; `applied` marker written only after success |
| 22 | CI doesn't build codex/browser or run the sandbox check | accept, except the Codex schema diff | CI builds all images and bundles and runs the Chromium sandbox test; the app-server schema diff is an open item, not Layer 1 |
| 23 | Slim carries the SDK's unused bundled CLI | accept (already fixed in d962728) | `npm ci --omit=optional` |
