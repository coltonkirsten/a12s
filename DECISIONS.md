# Build decisions: Layer 1 enhancements

Calls made while building `docs/a12s-layer1-enhancements-design.md` where the spec was silent, ambiguous or
contradictory. Each is the smallest sound choice; the final Astra pass reviews them with the code.

## S1: inbox storage and API

- **SQLite driver.** `modernc.org/sqlite` (pure Go): the Controller image builds with `CGO_ENABLED=0`, and cgo
  would have meant a C toolchain in the build stage and a libc-matched runtime. One `database/sql` connection
  (`SetMaxOpenConns(1)`) with `_txlock=immediate`: every inbox transaction is serialized in process, which is
  the ordering §4.5 relies on, and SQLite never answers `SQLITE_BUSY`.
- **Where `fleet.db` lives.** Volume `a12s-F-data` (label `a12s.data`), mounted at `/var/lib/a12s-data`,
  `A12S_DATA_DIR` in the Controller's env. With no `A12S_DATA_DIR` (tests) it lives in the state dir.
- **Error shape.** The spec says errors are `{error, fix}`. L1 already ships `{code, message}` as the JSON
  contract, so the enhancement keeps `code` (the stable error code) and `message`, and adds `fix` and an
  optional `details` map (`reason` for `not_idle`, `replacedBy` for `replaced`). Renaming `code` to `error`
  would break every L1 client for no gain.
- **Scoped tokens.** The spec names the actions (`send`, `inbox`) and the principal `token:<name>` but not how a
  token is made. Tokens are created by the owner (`POST /v1/tokens`, `a12s token create <name> --actions …
  --computers …`), stored as a SHA-256 hash in `fleet.db` (so they survive `down`/`up` like the rest of the
  Fleet's data), shown once, revoked with `a12s token rm`. Names are DNS labels of at most 63 characters (§4.13
  wire bounds). Actions: `send`, `inbox`, `status`, `events`, `power`, `stop`; Computers: a list or `*`.
  Everything else (`PUT /v1/fleet`, apply, shell, drives, tokens, the old turns endpoint) stays owner-only.
- **Reading another principal's Message.** A caller with only `send` that asks for (or withdraws) a Message
  whose `via` is not its own gets `404`, not `403`, so a token can't probe for ids.
- **Reservation.** A Message reserves its frame rendered with the widest index (`n="9999"`) and a 16-hex nonce,
  JSON-encoded with Go's default encoder (which writes `<`, `>`, `&` as six-byte escapes): the most the frame
  can weigh on the wire to computerd. The 4 KiB preamble reserve (§4.8) is subtracted from `maxBytes` before
  any admission check, `exceeds_inbox` included.
- **Limit source** (`computer`, `defaults`, `builtin`). Defaults are merged into each Computer at decode, so a
  Computer that repeats the default's exact value is reported as `defaults`. Harmless; the value is right.
- **`expires: 0` before delivery exists.** S1 has no drain, so now-or-never answers `409 not_idle` with the
  Computer's phase as the reason. S2 replaces the hook with the real admit-and-claim.
- **`a12s send --key`** is the idempotency key (default: a random UUID per invocation); `--replace` is the
  replace key, as §4.12's "a12s send --replace" says.
- **Purge counts.** `down --purge` lists the data volume with "inbox: N pending across M Computers, K held"
  when the Controller answers; offline it says the counts are unavailable rather than reading the volume.

## S2: delivery

- **Settlement reads a turn summary.** computerd gains `GET /v1/turns/{id}` → `{requestId, dispatched,
  resumeFailed, finished, error}`, computed from its journal. The spec says settle "from the journal
  (`GET /v1/events?turn=`)"; the summary is the same evidence in one call, so the drainer never re-reads a long
  turn's stream to find two events.
- **Wire JSON without HTML escaping.** Go's default encoder writes `<`, `>`, `&` as six-byte `\u00XX`; a Batch is
  full of angle brackets. The Controller sends computerd its Turn requests through `api.MarshalWire`
  (`SetEscapeHTML(false)`), and reservations are measured with the same encoder, so "the reservation is what
  computerd receives" holds exactly and the 4 KiB preamble reserve covers the worst-case escaped intro (512 × `[`
  → `&#91;`), which it would not under HTML escaping.
- **Keyed Messages reserve the widest `superseded` (ten digits).** A count carried over on a return (§4.12)
  then never changes a reservation, as gate 21 requires.
- **The continuation obligation lives in computerd's journal, not `fleet.db`.** computerd mints `c_<ulid>`,
  journals it in `harness.restarted`, reports it in `/v1/status` (recovered from the journal on restart) and
  clears it in the same append as the continuation's `turn.dispatched` (or `continuation.abandoned`). The
  Controller re-reads it at every idle boundary, so a second copy in `fleet.db` would only be a cache that can
  disagree. The replay safety the spec wants comes from the immutable `requestId`.
- **Which failures hold.** After dispatch, only `resume_failed` and a size refusal (`oversized`, recognized
  from the error text) hold the inbox; any other failed Turn is just a failed Turn, as in L1, and the next Batch
  is delivered. Before dispatch, a resume failure returns the Batch and holds `resume_failed`; three returns of
  one Message hold `undeliverable`.
- **Expiry is strictly past.** A row expires when `expiresAt < now`, so a now-or-never Message (`expiresAt =
  createdAt`) can be claimed inside its own enqueue transaction; the return routine expires a row once
  `now ≥ expiresAt`.
- **`expires: 0` serializes on the Computer's lock.** The check (settle open Batches, Controller phase,
  computerd status, hold), the insert-and-claim and the POST all happen under the Computer's lock, the same lock
  the drainer and the reconciler take.
- **The Message stream uses the journal's event envelope.** `GET …/inbox/{id}/events` emits `api.Event`
  frames (`seq` = `inbox_events` id, type = the inbox event, plus a first `snapshot`), so the CLI reuses its
  reconnecting `Last-Event-ID` reader unchanged. It also carries inbox-level `held`/`resumed`.
- **Fresh-session line.** `a12s prompt` prints `batch of 3: yours + <other senders>` from `turn.started`'s
  `inbox`, deduplicating sender labels.

## S3: crash correctness and journaling

- **Exit-gate test hooks are files, not flags.** computerd reads one-shot hook files from its root-only state dir
  (`/home/.a12s/test-hooks/`: `exit-after-accept`, `exit-after-dispatch`, `fail-before-dispatch`,
  `delay-before-dispatch` with seconds); the Controller reads `exit-after-claim` (optional delay in seconds)
  from its state dir. Arming one takes `a12s shell --root` or `docker exec` into the Controller, i.e. powers
  that can already kill those processes, so the hooks add no authority and need no build flag.
- **Journal flush placement.** The drainer seals owed `inbox.discarded` records after settling and before any
  claim, whenever the Computer is `Ready` or `Busy`; asleep, they wait in `fleet.db` (`journaled = 0`), and the
  first drain after a wake flushes them. Records are sent in chunks of about 1 MiB of bodies.
- **Prune.** Hourly, from the expiry loop: terminal rows ended more than 7 days ago and sealed in the journal,
  their events, and settled Batches. A row not yet journaled is never pruned.
- **`stop --hold` without an active Turn** just holds the inbox (the CLI calls `inbox/hold`).
- **`wake --clear-inbox`** returns the number discarded in `inboxCleared` on the power answer; the CLI sends one
  fresh requestId per Computer and reuses it on a retry.
