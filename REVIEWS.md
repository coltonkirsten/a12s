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

## Round 2 — full tree at 75707f1 (controller, computerd client, CLI; round-1 verification)

Verdict: not signed off. Round-1: 17 fixed, 1 rejection accepted (#20), 5 partial (#5, #6, #14, #21, #22) carried into new findings.
27 new findings (24–50): 10 blockers, 16 should-fix, 1 nit.

| # | Finding (Astra) | Decision | What changed |
|---|---|---|---|
| 24 | Prompt admission races sleep/restage (ordinary `up` could interrupt a turn) | accept | admission serialized with lifecycle mutations; closed before idle check and sleep |
| 25 | Moving an exclusive drive from busy A to B can create two writers | accept | B's attachment waits until A's old generation is observed gone |
| 26 | Stage/power/remove not read back through `Observe` | accept | state published only after observation agrees |
| 27 | Incomplete stage (preset pending) reported as success on same-generation retry | accept | bootstrap verified before the equal-generation return; cleanup failures reported |
| 28 | Controller restart never finishes pending removals | accept | startup reconciles owned Computers absent from the Fleet |
| 29 | Presets live only in memory; a restart stages without them | accept | preset tars persisted by content hash in the state dir; staging refuses without them |
| 30 | Staged egress policy applied before the idle boundary | accept | policy follows the active generation |
| 31 | Go SSE parser publishes an incomplete frame at EOF; cursor advances before decode | accept | one shared `internal/sse` parser; incomplete frames discarded; cursor advances after decode |
| 32 | Idempotent retry of a completed request marks the Computer Busy | accept | Busy derived from computerd status/events |
| 33 | A stop during the pre-dispatch credential restart still dispatches | accept | cancellation re-checked after restart and before `startTurn` |
| 34 | CI lacks Codex schema diff, conformance tests, release-triggered bundle builds | **reject** | those are the adapter-check doc's "recommended edits for the next design revision (not applied)", not Layer 1 spec; logged as open items |
| 35 | Refreshed tokens sit on the home drive until reclaim; reclaim can overwrite a newer secret | accept with a documented residual | reclaim within ~1s via `fs.watch`; never overwrite a newer tmpfs secret; the sub-second window is recorded as a Layer 1 limitation |
| 36 | Failed credential wiring still acknowledged | accept | wiring errors propagate; `ackedGeneration` unchanged on failure |
| 37 | Credential rotation skipped after Controller restart (empty hash) | accept | unknown hash ⇒ inject; hash set after success |
| 38 | IPv6 zone bypasses private-prefix checks | accept | zoned addresses rejected |
| 39 | Plain HTTP (port 80) forwarding allowed under enforced policy | accept | forwarding deleted; CONNECT only |
| 40 | computerd accepts any CA-signed client cert (a Computer cert could drive another computerd) | accept | Controller cert CN `controller` required; Computer certs lose client-auth except for leases |
| 41 | API token visible in `docker inspect` | **reject** | reading inspect requires the Docker socket, which is already full control of every Computer; the token adds no exposure |
| 42 | Leases not authorized against drive access | accept | caller must have the `shared-write` drive attached writable |
| 43 | Asleep status can show a stale session | accept | `session.changed`/`resume.failed`/`lifecycle` consumed; final status persisted before stop |
| 44 | `a12s up` exits 0 with per-Computer errors | accept | errors printed, non-zero exit |
| 45 | `logs` ends replay on a 2s idle timer | accept | computerd emits `event: head` after replay; `logs` stops there |
| 46 | `*` row warning read from local YAML | accept | egress mode comes from Controller status |
| 47 | Comma in a mount path injects a drive entry → journal dir handed to the agent | accept | mount paths restricted at validation; computerd refuses reserved paths |
| 48 | Gate script `eval`s agent output on the host | accept | no `eval`; checks are functions over files |
| 49 | Gate assertions not scoped to the new turn | accept | per-turn cursors; text must come from the stream, not the echoed prompt |
| 50 | Unused helpers; two SSE parsers | accept | helpers deleted; one parser |

## Round 3 — sign-off round at 30f50c1

Verdict: not signed off. Round-2 verification: 20 fixed, #41 rejection accepted, #34 rejection not accepted, 5 partial (#24, #26, #30, #31, #35); 2 new (#51 blocker, #52).

| # | Finding (Astra) | Decision | What changed |
|---|---|---|---|
| 24/26 | A failed computerd Status falls back to the cached phase; wake publishes Ready without Observe | accept | unknown status ⇒ not idle; wake confirms generation/power through Observe |
| 30 | Fleet `egress.enforcement` applied immediately, not with the generation | accept | enforcement is part of the active policy and generation hash |
| 31 | A malformed event followed by a valid one is skipped | accept | decode failure stops the stream and reconnects from the last good cursor |
| 34 | Release-triggered bundle builds and a conformance test are §5 requirements | accept (rejection withdrawn) | `bundles.yml`: daily/dispatch discovery of upstream releases, build, conformance check, push to ghcr with `GITHUB_TOKEN`; `conformance.sh` asserts the flags and app-server methods the adapters use; the Codex schema diff stays Layer 2 |
| 35 | Refresh writes could land on the home drive; reclaim races injection | accept, with evidence | Codex 0.154.0 `storage.rs save()` writes in place (`truncate+write+create`, follows the symlink → tmpfs); Node `writeFileSync` likewise; reclaim is a fallback, now serialized with wiring; raw `sk-ant-oat…` → `CLAUDE_CODE_OAUTH_TOKEN` (no file at all) is the recommended Claude credential |
| 51 | Exclusive-drive swap between two idle Computers deadlocks | accept | obsolete attachment released at idle, detachment observed, then the new generation staged |
| 52 | A corrupt cached preset cannot be repaired by `a12s up` | accept | existing cache entry hash-validated or replaced atomically |

## Round 4 — sign-off round at c86b49f

Verdict: not signed off; all round-3 items verified fixed (#35 main path fixed with the acknowledged flock residual; #51 partial → #53). 3 new findings.

| # | Finding (Astra) | Decision | What changed |
|---|---|---|---|
| 53 | A swap submitted while both holders are busy never converges after the turns end (premature holder check in `applyStaged`) | accept | holder check removed; admission closed, idle confirmed, `restage` releases the obsolete attachment |
| 54 | Recycled networks can inherit the proxy's cached address→Computer mapping (a Computer could get its peer's policy) | accept | mapping invalidated before network removal, refreshed before a replacement serves, fail closed without a verified mapping |
| 55 | A malformed first journal event can be skipped because the initial cursor is empty | accept | cursor initialized from the validated `head` frame |

## Round 5 — sign-off round at 0af4da0

Verdict: not signed off; #53 and #55 verified fixed; #54 reopened as #56.

| # | Finding (Astra) | Decision | What changed |
|---|---|---|---|
| 56 | A proxy-map refresh that starts after invalidation can publish the old `IP → A` mapping after B reused the address (epoch check doesn't cover the whole removal) | accept | refresh read/publish serialized with the whole invalidate-and-remove operation under one mutex; deterministic test holds the stale snapshot until after replacement verification |
| 57 | Unknown-mapping denials younger than 1s are not logged | accept (small) | both return paths log |

Follow-up after round 5 (not an Astra finding): the controller package tests were flaky under full-package runs. Two causes, both fixed (`dbc10d9`, `1d1faba`): a lost wakeup when an exclusive drive is freed (a #51 follow-up, a real code fault), and restart tests that ran two Controllers on one state dir (the Controller now tracks its background goroutines and `Wait`s for them on shutdown). `go test ./internal/controller/ -count=10` and `-race -count=3` are green.

## Round 6 — sign-off at 6dbd03a

**Signed off.** #56, #57 and the follow-up verified fixed; no Layer 1 blocker. Two Layer 2 should-fix notes, recorded in PLAN.md "Open items": #58 (detached fleet/apply/power workers are not counted by the shutdown tracker) and #59 (the #56 race test gives the competing refresh a 50 ms window; synchronize it explicitly).

Totals: 6 rounds, 59 findings (1–59): 55 accepted and fixed, 2 rejected and accepted by Astra (#20, #41), 1 rejection withdrawn and implemented (#34), 1 accepted with a documented residual (#35), 2 carried to Layer 2 (#58, #59).

Follow-up after sign-off (CI on Linux, not an Astra finding): `TestPresetsSurviveRestart` failed about 1 in 20 on ubuntu. Root cause was a real start-up ordering bug — the Controller's resume pass ran in a goroutine that took the apply lock late, so an `a12s up` landing right after a restart could be overtaken by the resume. `Start` now holds the apply lock until the resume pass finishes (`007fd6c`); 0 failures in 200 Linux runs afterwards, new `TestApplyWaitsForTheResume`. Two computerd tests also assumed macOS (coarse Linux mtimes; `/tmp` is a reserved drive path) and were fixed (`8219625`). The exit gate was re-run on the final tree: 17/17.

## Closeout — 2026-10-06 (PR #3, Layer 1 leftovers #58, #59, #35 and the setup-token lab example)

Two rounds, the cap Colton set. Raw reports: `~/raven-v2/deliverables/79166166dd4a/astra_r{1,2}.md`.

### Closeout round 1 — at 05f1178

Verdict: not signed off. 5 findings.

| # | Finding (Astra) | Decision | What changed |
|---|---|---|---|
| 60 | spawn's context check and `bg.Add` race `Wait` (a worker admitted after the last one exits; zero-to-one Add during Wait) | accept | `Wait` sets `stopping` under `bgMu`; `spawn` checks it and Adds under the same mutex; `TestSpawnRefusedOnceWaitBegins` |
| 61 | The injected file's mtime is its staging time, so a refresh written while the injection was staged reads as newer and is reclaimed over it | accept | the published file's mtime is set under the lock (finished in #65) |
| 62 | computerd could create `.lock` 0644 (older Controller), letting the agent hold the flock | accept | both sides create or repair `.lock` as root 0600 (writeScript; computerd `secureLock`, `O_NOFOLLOW`) |
| 63 | The lock test used a 500 ms window | accept | handshake: the holder waits for the reclaim's temp file |
| 64 | The shutdown test never joined the detached caller | accept | the caller's result is received before the worker is released |

### Closeout round 2 — at 90c25bd

Verdict: not signed off. #60, #62, #63, #64 verified fixed; #61 partial, reopened as #65.

| # | Finding (Astra) | Decision | What changed |
|---|---|---|---|
| 65 | `touch` before `mv`: a refresh written between them still reads as newer | accept | `mv` then `touch` of the published file, both under the lock (`7bb5dad`); a reclaim that stamped before the touch fails its locked compare-and-swap. Verified in a slim container: published mtime after the rename; not re-reviewed by Astra (two-round cap) |

Proof for the closed tree (`7bb5dad`): CI https://github.com/coltonkirsten/a12s/actions/runs/37553747519 (Go, computerd on
Linux incl. the flock test), exit gate 17/17 (`EXIT-GATE.md`, `docs/exit-gate-2026-10-06.log`), bundles
https://github.com/coltonkirsten/a12s/actions/runs/37549825263 (workflow_dispatch: ghcr login, claude-code 2.1.292 built,
conformance OK on amd64 and arm64/QEMU, pushed) and https://github.com/coltonkirsten/a12s/actions/runs/37470145892
(schedule: claude-code 2.1.291 and codex 0.160.1, same path).

### Post-closeout verification — Astra re-check of #65

Astra's one-round check of `7bb5dad` (the round the two-round cap skipped) returned two findings:

| # | Finding (Astra) | Decision | What changed |
|---|---|---|---|
| 66 | A `touch` that fails after the `mv` leaves the published secret with its pre-publication mtime, so a staged refresh reads as newer and is reclaimed over it | accept | on a failed touch the published file is removed and the write fails (`writeScript`, both branches); `TestWriteScriptTouchFailureRemovesPublishedFile` |
| 67 | The no-flock fallback (reclaim and injection both unlocked) can still lose the injected secret | already documented | the #35 microsecond window for custom images without `flock(1)`; PLAN.md ("Custom images without `flock(1)`"); slim and browser tiers ship util-linux |

Round 3: sign-off (Astra, `gpt-6-astra`, on the #65 fix and #66; #67 accepted as the documented no-flock limit). #65 is now re-reviewed.

**Layer 1 DONE 2026-10-06.** Totals across Layer 1: 8 rounds, 65 findings; #58 and #59 closed here; #65 is fixed and re-reviewed (post-closeout round 3: sign-off; #66 and #67 raised there).

## Enhancements final review — the whole Layer 1 enhancements diff at 9237e1b (S1–S8)

One pass, as asked (Astra, `codex exec -m gpt-6-astra`, xhigh, read-only), over `git diff 089eb7c..9237e1b`
against `docs/a12s-layer1-enhancements-design.md` and DECISIONS.md. Raw report:
`~/raven-v2/deliverables/a12s-build/astra/final.log`. Verdict: NOT YET, 18 findings (6 blockers, 12 should-fix).
All 18 accepted and fixed in the PR that follows this review.

| # | Finding (Astra) | Decision | What changed |
|---|---|---|---|
| 1 | Image `HEALTHCHECK` inherited: image code runs as root (a12s-init's user) and in the publish forwarder | accept (blocker) | `Healthcheck: NONE` on Service, forwarder, login and Computer containers |
| 2 | Bridge missing from the named-egress deny list when the engine read fails | accept (blocker) | last verified bridge remembered; without one, named egress fails closed |
| 3 | A certificate outlives a Service that died while the Controller was down | accept (blocker) | registry reconciled against running containers at start; the listener also requires the principal's container to be up (fail closed) |
| 4 | A failed registry delete leaves a stopped principal authorized | accept | revocation recorded in memory first; a failed write still refuses |
| 5 | A VOLUME under a tmpfs path still gets an anonymous Docker volume | accept | every image VOLUME without an exact mount is mounted as tmpfs too |
| 6 | `creds set` echo-off ran `stty` on /dev/null; the token was echoed | accept | `stty` on the terminal, errors checked, restored on return; refuses to read if echo can't be turned off |
| 7 | Return could replace a row with an already-overdue one, delivering neither | accept (blocker) | the return routine expires all overdue pending rows first |
| 8 | The first POST ignored the expiry fence | accept | every POST past `notAfter` is lookup-only |
| 9 | Holds owed after dispatch (oversized, resume.failed) were missed when settlement ran at dispatch | accept | after `turn.finished`, the delivered Batch's Turn summary sets the hold |
| 10 | A continuation accepted but not dispatched before a crash was owed forever, wedging the inbox | accept (blocker) | computerd abandons it at startup (`continuation.abandoned`), the Controller holds `resume_failed` |
| 11 | `inbox resume --new` didn't cancel the owed continuation | accept | the drainer cancels it (`POST /v1/continuation/cancel`) when a new-session override is pending; a cancelled abandonment doesn't re-hold |
| 12 | A Batch claimed just before a hold could POST after it | accept | holds take the admission gate; `postBatch` re-checks the hold inside it |
| 13 | `wake --clear-inbox` missed rows of a claim that came back later | accept | the return routine discards rows a clear accepted during the claim covers |
| 14 | Deferred journaling budgeted raw bodies; escaped chunks could exceed 2 MiB forever | accept | chunks budget the serialized records |
| 15 | An enqueue could commit after auto-sleep's pending check and lose to the sleep | accept | auto-sleep closes admission, then re-checks pending |
| 16 | `sleep; down; up` brought the Computer back awake | accept | new records take desired power from fleet.db's sleep mark; the mark is written before the sleep |
| 17 | A Service reattached after a Controller restart stayed Provisioning | accept | reattach goes through Starting so health can make it Ready |
| 18 | A delayed death event revoked the restarted instance's certificate and stopped its watchers | accept | revoke and stop only after confirming the instance is still dead |
