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
