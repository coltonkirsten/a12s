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

## S3b: power policy

- **The idle clock is a 1-second tick, not per-Computer timers.** The power loop checks every Computer's policy
  each tick (idle → auto-sleep; asleep → wake-on-message). The tick also covers every "later moment that makes
  a Message eligible" (rate window reopening, credentials loaded, hold lifted) without wiring each one; enqueues
  and drains additionally evaluate wake at once. Auto-sleep re-checks idleness under the Computer's lock,
  including computerd's status (turn, continuation) and attached exec sessions.
- **Attached sessions** are counted from Docker (`ContainerInspect.ExecIDs`, running ones): any `a12s shell`
  or other exec keeps the Computer awake. The Controller's own credential-injection execs are momentary.
- **Test speed.** `A12S_TEST_TIME_SCALE` (≥ 1, passed from the CLI's environment to a newly created Controller)
  divides `sleepAfterIdle` and the rate window, for the exit gate. Absent in production.
- **Suppression is recorded per sleep and reason** (`suppressed` in fleet.db, reset by every sleep): `manual_sleep`,
  `credentials_not_loaded`, `wake_failed`, `rate_limit` (with `until`). No suppression is recorded while a wake
  is in flight.
- **`service:<name>` in `wakeOnMessage.from`** is refused until Services exist (S4 adds them); the warning for a
  listed principal without a `send` grant arrives with grants (S5).
- **Manual wake clears both the manual mark and a `wake_failed` pause; `a12s up` clears the pause.** An auto-wake
  clears the "slept by" mark (the Computer is awake) but not a pause.

## S4: the Service primitive

- **a12s-init is the `a12s` binary.** The Docker driver copies the running executable into a read-only
  `a12s-F-init` volume under `<sha256 prefix>/a12s` (so a new Controller never swaps the binary under a running
  Service) and mounts it at `/a12s/init`. The Service's entrypoint is `a12s service-init -- <image entrypoint+cmd>`.
  It hands an empty state mount and the manifest's tmpfs paths to the manifest uid, waits for `.generation`,
  exports `config.env`, then `setgroups/setgid/setuid` to the manifest uid (uid 0 refused) and execs. Leaving
  uid 0 clears every capability and `no-new-privileges` keeps them away; Docker's `--init` (tini) stays PID 1, as
  for Computers. Verified on real Docker: the image's processes run as 1000 with `CapEff 0`.
- **Injection needs no shell in the image.** The Controller writes secrets (manifest uid, 0400), `config.json`
  and `.generation` through `a12s service-write` (atomic temp + rename, only under `/run/a12s`) run by exec as
  root, instead of the Computers' `sh` script; stock and distroless images work the same.
- **Covered VOLUMEs.** A VOLUME is covered when it *is* the state mount, or is a tmpfs path or under one. A
  VOLUME below the state mount would get an anonymous Docker volume nobody tracks, so admission refuses it with
  `set manifest.state.at to <v>, or list it in manifest.tmpfs`.
- **`publish:` without opening the Service.** A Service's network is internal, so it cannot publish a port.
  Each published Service gets a forwarder container (`a12s-F-<svc>-publish`) that runs only `a12s
  publish-forward` (uid 65534, no capabilities, read-only) on the Fleet bridge with the host port bound, and
  joins the Service's network to forward to it. The Service gains no route out. It starts and stops with the
  Service; it is not restarted on its own if it dies (logged as open work).
- **Restarts never give up.** A Service that exits is restarted through the full start path with doubling
  backoff capped at 5 minutes; `restarts` counts them since the last operator action (`up`, `wake`). Unhealthy is
  reported, never restarted.
- **Missing credentials.** A static Service credential with no binding is an admission error (`credential X has
  no binding  fix: a12s creds set X`); one the Service's own manifest login produces leaves only that Service in
  `Error: <svc> needs a login  fix: a12s service login <svc>`, reported by `up` as a warning, not a failure. The
  CLI sends whatever Service credentials are bound and lets the Controller decide.
- **Pins.** `fleet.lock` `services:` is keyed `<svc>@<ref>`, append-only. `a12s lock update services/<svc>`
  drops that key client-side and sends `refresh: [svc]`; the Controller pulls the ref if a registry has it
  (a locally built image keeps its local ID) and pins the result.
- **Docker event filters AND their label values**, so the watch no longer filters by role; `toEvent` keeps
  Computer and Service events and drops the rest (forwarders, extractors).
- **Not yet (S5):** typed certificate SANs and the active-certificate registry, grants, `.a12s` routing, MCP
  wiring. A Service already reaches the internet only through the egress proxy with its own `egress.allow`;
  the proxy identifies it by network exactly as it does a Computer.

## S5: identity, grants and reachability

- **Typed identity.** Computer and Service certificates carry `a12s://<fleet>/<kind>/<name>/<generation>` as
  their only URI SAN; Computers keep their DNS SAN (computerd's server name), Services get none. Both stay
  server-auth only, so neither can act as a client of a computerd. The mTLS listener accepts a certificate only
  when its serial and generation are the live registry entry (`cert_registry` in fleet.db).
- **Registry lifecycle.** An entry is written as the certificate is issued at each start (after the container is
  started, before the files are injected) with the active generation's `grants.send`, and deleted before every
  stop, removal, release, and on a death before the restart. A credential rotation on a running Computer or
  Service re-injects secrets only: it keeps the loaded certificate (computerd reads its TLS files once), so the
  live serial stays valid. A Computer started before this change has no entry until its next start.
- **Grants live in the generation.** `grants.send` and the resolved `uses` endpoints go into the container env
  (`A12S_SEND`, `A12S_USES`, `A12S_EGRESS_TCP`), only when non-empty so existing hashes don't move; a grant
  change therefore restages (a Service at once, a Computer at its idle boundary), and the registry snapshot is
  always the active generation's.
- **MCP names.** A used Service with one MCP port is registered as `a12s-<svc>`; with several, each as
  `a12s-<svc>-<port>` (the spec names both forms; this keeps gate 12's `a12s-db`).
- **The stdio bridge** (`computerd/dist/mcpbridge.js`) is a small Streamable-HTTP client written against the
  wire format (JSON or SSE answers, `Mcp-Session-Id`), sending plain-HTTP absolute-URI requests through the
  proxy. It does not open the optional server-initiated GET stream.
- **TCP forwarders.** computerd listens on `127.0.0.1:0` per used TCP port; a12s-init binds the listeners as root
  and hands them to a child `a12s service-forward` at the manifest uid before it execs the image, so the
  forwarders outlive the exec. Each connection is a CONNECT to `<svc>.a12s:<port>` (or `<name>.egress.a12s`).
- **Named egress checks, in order:** loopback/link-local/metadata/multicast/unspecified, then the Fleet's own
  pools (the internal subnet pool and the /16 of the Controller's Fleet-bridge address), with no Fleet field
  read; then private ranges only inside `egress.allowPrivate`; the resolved address is the one dialed.
- **Denials.** A `.a12s` refusal or a 503 for a Service that is not Ready is journaled on the calling Computer
  as an egress `diagnostic` with the reason (`scout cannot reach db: computers.scout.uses does not list db`,
  `db is Asleep`), or audited on a calling Service. A refused send is a `403 no_grant` with the fix, audited
  for a Service and journaled as a diagnostic for a Computer.

## S6: credentials and login

- **The login container is a12s holding a memory-only container open.** `POST /v1/services/{s}/login` starts
  `a12s-F-<svc>-login` from the pinned image as the manifest uid, no capabilities, read-only rootfs, on the
  Service's own network (so the proxy applies the Service's egress policy). `/run/a12s` (holding `HOME`,
  `XDG_*` and `$A12S_LOGIN_OUT`), `/tmp`, every image VOLUME, the manifest's tmpfs paths and the state path are
  tmpfs; no state drive is mounted. Its process is `a12s login-hold`. The CLI execs the manifest command into it
  on the terminal, then reads `$A12S_LOGIN_OUT` with `a12s login-read` over the exec stream, writes
  `~/.a12s/<fleet>/<credential>` atomically 0600, binds it in the bindings file, and deletes the container.
  The credential never passes through the Controller. `a12s up` (or `wake`) then injects it.
- **`a12s creds set <name>`** lands here (not S8) because login shares its storage: the value comes from stdin
  (not echoed on a terminal), goes to `~/.a12s/<fleet>/<name>` (0600, atomic) and is bound as `{file: …}`,
  keeping the rest of the bindings file. `down` never deletes these files (it removes only `controller.json`).
- **Reauth.** An HTTP health answer `{"credential": "reauth"}` makes the Service Unhealthy with
  `credential: reauth  fix: a12s service login <svc>` on its status line. Rotation is not written back (open
  question 3: "log in again after a sleep").
- **Static credentials** were already injected through a12s-init in S4 (`/run/a12s/secrets/<name>`, the manifest
  uid, 0400); a Service's `config.env` maps them (`POSTGRES_PASSWORD_FILE: /run/a12s/secrets/postgres`).
- The controller image now builds with a Go build-cache mount, so each rebuild adds only the binary to the
  build cache (the disk on mac-mini is tight).
