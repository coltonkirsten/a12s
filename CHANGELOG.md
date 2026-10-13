# Changelog

Breaking changes are allowed before 1.0 when they make a12s simpler; each one is listed here with what to do.

## Unreleased

- Added: `attach` mounts a folder of this machine into an agent computer at `/home/agent/<name>`
  (`attach: { project: { host: ../myapp, readOnly: false, keep: [node_modules] } }`), so an agent works on your
  project in place. `keep` paths live on drives of their own (`keep-…` in `a12s drive ls`) and never reach the
  folder. Only the owner sets it (a prefix token gets `403 forbidden`, also for changing the image of an agent computer
  with one), and not in `defaults`; `a12s up` refuses `~`, `~/.a12s` and `~/.docker`. `a12s up` resolves a
  relative or `~/` host against fleet.yaml; `a12s export` writes it back. A folder Docker can't see (missing, or not
  shared with Docker Desktop) puts the agent computer in Error with the fix. Custom images need a rebuild FROM the
  new slim, whose computerd hands kept drives to the agent. See docs/fleet-file.md#attached-folders.
- Changed: background work survives the end of a Turn. Claude Code now runs as one process per session instead of
  one per Turn, so a sub-agent or Bash command the agent runs with `run_in_background` keeps going after its Turn
  ends, and when it finishes Claude Code starts a Turn by itself to report: `turn.started` has
  `reason: "background"` and no Messages (`turns()` shows it as `reason`). Background tasks are journaled as
  `task.started` / `task.finished`, and status shows `background: {tasks}` while any run. Meanwhile the agent
  computer stays Ready, so a Message runs at once, but it is not idle: `sleepAfterIdle` and config changes that
  restart it wait for the background work to end. Codex commands still running at the end of a Turn are tracked
  the same way (Codex never starts a Turn by itself). What ends the session and its background work: `a12s stop`
  (and `stop()`), sleep, `restart_harness`, a `--fresh` Message, applying a config change, the agent computer
  stopping. `a12s stop <name>` and `stop()` without a Turn id stop whatever runs, background work included, through
  the new `POST /v1/agent-computers/{c}/stop`; `a12s stop --json` now prints `{computer, turns, background}`
  instead of `{computer, turn, status}`, so scripts reading `turn` read `turns`. See
  docs/concepts.md#background-work.

## v0.7.1 (2026-10-13)

- Fixed: `a12s creds set` (or any command that starts the Controller) before the first `a12s up` on a new install or
  release failed with "controller image … not found locally"; it now pulls the Controller image like `up` does.
- Fixed: the README `a12s init` writes said `creds set` is once per machine; credentials belong to the Fleet's
  Controller and `a12s down` removes them. It also names `openai` for `init --harness codex`. `a12s init`'s help no
  longer mentions a .gitignore.
- Docs: Linux needs Docker Engine 28 or later (README, llms.txt); llms.txt names the package `a12s ui` runs
  (`github:coltonkirsten/a12s-console`); `a12s drive ls` shows no sizes.

## v0.7.0 (2026-10-13)

A deletion release: the v0.4 aliases, the gateway dashboard, rarely used flags, `home`, `autonomy`, work drives,
`a12s service login`, `a12s apply` and the Service networking extras are gone, and llms.txt is down to the core path.

Upgrading: install the new release, rebuild custom images FROM `ghcr.io/coltonkirsten/a12s/slim:0.7.0` under new
tags, and `a12s up`. The new Controller drops the fields this release removes from the Fleet it stored (`home`,
`autonomy`, `inbox.intro`, work `drives`, the top-level `egress`, a Service's `publish` and `egress.tcp`, a manifest's
`login`) and keeps the rest running. A write that sets one of them is refused, so take them out of fleet.yaml and out
of the specs your code writes.

### Breaking
- **The v0.4 names are gone.** A Fleet file's `computers:` key is refused: rename it to `agent_computers:`. The
  `/v1/computers/...` paths are gone: use `/v1/agent-computers/...` (rebuild a Service built with svckit v0.2.1 or
  earlier against the current svckit). `GET /v1/status` no longer carries `computers` (use `agentComputers`), events
  and SDK Turns no longer carry `computer` (use `agentComputer` / `agent_computer`), and `GET /v1/events` no longer
  reads `?computers=` (use `?agentComputers=`). `a12s token create --computers` is gone (use `--agent-computers`), and
  `a12s gateway --computers` is now `--agent-computers`.
- **The name-first clients are private.** TypeScript `connect()` and `A12sClient`, and Python `connect()`,
  `connect_async()`, `Client` and `AsyncClient` are no longer exported: use `Fleet.connect()` / `a12s.Fleet()` /
  `a12s.AsyncFleet()`, whose agent computer handles have the same methods (`fleet.agentComputers.get(name).send(...)`).
  TypeScript `fleet.client` keeps `url` and `request(method, path, body?)` only; for `inbox`, `message`, `withdraw`
  and the rest, use the agent computer handle. `A12sClientOptions` is now `ConnectOptions`.
- **`a12s gateway` serves only your app.** The built-in dashboard is gone, and `a12s gateway` with no `--web`, `--dev`
  or `-- <command>` is a usage error. For a dashboard, run `a12s ui`.
- **Rarely used flags are removed:**
  - `up --asleep`: run `a12s up`, then `a12s sleep <name>`. `PUT /v1/fleet` takes no `asleep`.
  - `up --memory-only-credentials`: the Controller always keeps credential values in its state volume (0600), and
    `A12S_MEMORY_ONLY_CREDENTIALS` is ignored. `GET /v1/status` (SDK: `status()`) no longer returns
    `credentialsLoaded`.
  - `stop --hold`: run `a12s inbox hold <name>`, then `a12s stop <name>`. The Turn stop route takes no `?hold=true`.
  - `wake --clear-inbox`: run `a12s inbox clear <name>`, then `a12s wake <name>`. The power route takes no
    `clearInbox` or `requestId` and returns no `inboxCleared`; SDK `wake()` takes no `clearInbox` / `clear_inbox`.
  - `inbox clear --orphan`: `a12s inbox clear <name>` now clears a removed agent computer's inbox as well. The clear
    route takes no `orphan`.
  - `logs --since`: `a12s logs <name>` replays the whole journal; filter on the seq column.
  - `shell --root`: use `docker exec -u 0 <container>`. The shell routes take no `root`.
  - `send --key`, the SDKs' `idempotencyKey` / `idempotency_key` send option and svckit's `Enqueue.IdempotencyKey`:
    the CLI and SDKs set a fresh key on every send themselves; the HTTP API still accepts `idempotencyKey`.
- **`credentials` add to the defaults' list** instead of replacing it: with `defaults.credentials: [anthropic]`, an
  agent computer's `credentials: [github]` now gets both. Other lists still replace. The write no longer warns about
  a credentials list missing the harness's credential. To keep an agent computer off a default credential, take it
  out of `defaults` and list it on the agent computers that need it.
- **`home` is gone** (`home.size` and `home.preset`). Docker never enforced `home.size`: delete it. Put a preset's
  files in `files:` (`{path: text}`) and its `CLAUDE.md` (`AGENTS.md` for Codex) in `instructions:`. `a12s export`
  now writes instructions and files into fleet.yaml instead of `presets/<name>/`; delete the `presets/` directory
  once you've moved its files. Agent computer status has no `homeSize`, and home drive rows no `size`.
- **`autonomy` is gone.** Every agent computer runs as `autonomy: full` did (Claude Code `bypassPermissions`, Codex
  approval `never`); `restricted` is removed. Delete `autonomy:` from fleet.yaml. Agent computer status has no
  `autonomy`.
- **`inbox.intro` is gone.** Every Batch starts with the same fixed intro line before the safety block. Delete
  `intro:` from `inbox:`.
- **Only the owner sets `security.seccomp`.** A prefix token's write that adds, changes or drops an agent computer's
  own seccomp profile is `403 forbidden`; set profiles with the owner token (`a12s up`, or the SDK as owner).
- A Controller's state from before v0.4.0 is no longer migrated: run `a12s up` with your fleet.yaml again.
- **Work drives are gone.** The top-level `drives:` (`exclusive`, `shared`, `shared-write`), an agent computer's
  `drives: [{name, at, readOnly}]` and the agent's `lock` / `release` system tools are removed, and a Fleet that names
  them is refused. Agent computers share work through Messages or a Service. Home drives, Service state drives and
  `a12s drive ls` / `rm` stay; `drive ls` no longer lists work drives and its rows have no `access`, and status rows
  have no `drives`. A work drive's volume (`a12s-<fleet>-drive-<name>`) keeps its data after
  the upgrade: copy out what you need, then `docker volume rm` it (`a12s down --purge` also removes it).
- **`a12s service login` is gone**, with the Service manifest's `login {command, produces}`, the
  `/v1/services/{s}/login` routes and the `{"credential": "reauth"}` health answer: set a Service's credential with
  `a12s creds set <name>` like any other.
- **`a12s apply` and `POST /v1/apply` are gone.** A staged change applies at the agent computer's next idle moment;
  to apply it now, end the Turn with `a12s stop <name>`.
- **Service networking extras are gone:** a Service's `publish:` (host ports; a12s opens no inbound ports, so receive
  webhooks on your own server and send with the SDK), a Service's `egress.tcp`, and the top-level `egress:`
  (`allowPrivate`, `enforcement`, `unenforced`). Egress is always enforced: on a Docker Engine older than 28, upgrade
  it. `GET /v1/status` no longer carries `egressEnforcement`, and Service status no longer carries `publish`.

### Changed
- llms.txt is cut from 726 lines (v0.6.0) to 451: one SDK block for TypeScript and Python (with the Python naming
  rules), a short HTTP section next to the generated API surface, and shorter event and error lists (docs/api.md has
  them all). The Go Service walkthrough, the gateway example page and the hand-off recipe moved to the cookbook:
  [go-service](https://github.com/coltonkirsten/a12s-cookbook/tree/main/examples/go-service),
  [gateway-page](https://github.com/coltonkirsten/a12s-cookbook/tree/main/examples/gateway-page),
  [hand-off](https://github.com/coltonkirsten/a12s-cookbook/tree/main/examples/hand-off). docs/services.md points
  at go-service instead of the v0.2.1 reference Services, whose svckit no longer reaches the Controller.
- llms.txt showed `env:` as a Service key, which the Fleet refuses: a Service's environment goes in
  `config: { env: {...} }`.

## v0.6.0 (2026-10-13)

The live Fleet is the source of truth. fleet.lock, the bindings file and drift detection are gone: `a12s up` is a
plain apply of fleet.yaml (which stays optional), `a12s export` writes the live Fleet back, and credentials live only
in the Controller.

Upgrading: install a12s 0.6.0, rebuild custom images FROM `ghcr.io/coltonkirsten/a12s/slim:0.6.0` under new tags,
then `a12s up`. Delete `fleet.lock` and `a12s.bindings.yaml` from your project. Credentials the running Controller
already holds stay; on a new Controller, `a12s creds set <name>` each one again.

### Breaking
- **fleet.lock is gone.** The Controller pins the image each image, bundle and Service ref names when first used, in
  its own state, and keeps running it; a new tag is how a rebuilt image rolls out (`a12s service status <svc>` shows
  the pin). If the pinned image is gone, the ref's current image is used. The `a12s:` release pin is gone: a binary
  runs its own release's images. `a12s lock update` is removed; give the image a new tag instead.
- **The bindings file (`a12s.bindings.yaml`) and `--bindings` are gone.** `a12s creds set <name>` sets the value on
  the Fleet's Controller, starting it if none runs, and keeps no copy on the host (no `~/.a12s/<fleet>/<name>`);
  `a12s service login` does the same with what the login produces. `a12s up` and `a12s wake` no longer send
  credentials. With `--memory-only-credentials`, set them again after a Controller restart.
- **No drift.** `GET /v1/fleet` no longer returns `drift` (SDK: `fleet.get()` has no `drift`), `a12s up` no longer asks
  `apply anyway?` and `--force` is removed: it makes the live Fleet match the file, removing what the file doesn't
  list. To keep what code changed, `a12s export` first (`git diff` shows it). `a12s diff` is removed; `export` takes
  no `--force` and overwrites what differs.
- **`PUT /v1/fleet`** takes no `lock`, `refresh` or `file`, and its answer has no `lock`.
- `a12s init` writes fleet.yaml and a README only.

## v0.5.0 (2026-10-12)

The obvious guess is now the right one: a plain sleep is woken by a listed sender, `send()` hands back the Turn
that answers it (`prompt` is gone), naming a credential before it has a value waits instead of failing, the npm
package is the only way into a Fleet from a browser, and sleepAfterIdle takes seconds.

Upgrading: set `a12s: v0.5.0` in fleet.lock and delete its `slim` pin, rebuild custom images FROM
`ghcr.io/coltonkirsten/a12s/slim:0.5.0` under new tags (admission needs the same minor as the CLI), then `a12s up`.

### Breaking
- **`a12s gateway` no longer serves the SDK at `/a12s.js`, and the npm package drops its `./browser.js` export.** A
  page imports `@coltonkirsten/a12s` through a bundler: `npm install @coltonkirsten/a12s vite`,
  `import { Fleet } from "@coltonkirsten/a12s"` in your script, and `a12s gateway -- npx vite`. `--web <dir>` serves
  a built app (`npx vite build`, then `a12s gateway --web dist`).
- **`send()` returns a handle whose `wait()` gives the Turn that delivers the Message; `prompt()` and `a12s prompt`
  are gone.** `wait()` is keyed on the Message, so it can't miss a fast Turn, even when called after the Turn ended.
  What to do: `agent.prompt(x, opts)` → `agent.send(x, opts).wait({ onEvent, signal })` (Python:
  `agent.send(x, **opts).wait(on_event=…)`; async: `await agent.send(x).wait()`); `const m = await agent.send(x)`
  still gives the Message, and `m.wait()` its Turn. TypeScript's `PromptResult` is now `Reply` (Python
  `a12s.PromptResult` → `a12s.Reply`) and `PromptOptions` is gone (send options plus `WaitOptions`). CLI:
  `a12s prompt c "x"` → `a12s send c "x" --wait`, and its `--new` → `--fresh`.
- **A plain sleep leaves the power policy in charge.** `a12s sleep <c>`, SDK `sleep()` and
  `POST …/power {"state":"asleep"}` now sleep an agent computer like sleepAfterIdle does: a Message from a sender
  listed in wakeOnMessage wakes it. `--allow-wake` / `allowWake` / `allow_wake` are removed. What to do: to keep it
  asleep until a manual wake, use `a12s sleep <c> --hold`, `sleep({ hold: true })` or `sleep(hold=True)`
  (`{"state":"asleep","hold":true}`); `a12s status` shows it as `Asleep (held)`. Agent computers already asleep from
  a manual sleep under v0.4 are now in the policy's charge.

### Changed
- **sleepAfterIdle accepts 1s to 7d** (e.g. `5s`); the 1m floor is gone.
- **Naming a credential that has no value yet no longer fails**, so the order of `creds set` / `credentials.set` and
  `a12s up` / `create` doesn't matter. The write succeeds with a warning (`agent_computers.job-1: credential dbname has
  no value yet; it starts when you set it (a12s creds set dbname)`), and the agent computer or Service waits in
  `Provisioning` with `waiting: {credentials, fix}` in its status, `a12s status` and the `agent_computer.phase` /
  `service.phase` events. Setting the value starts it; `a12s up` and SDK `create()` return instead of waiting for
  it. A Service missing a credential now waits in `Provisioning` instead of `Error`. The error code
  `missing_credential` is gone. What to do: nothing.

## v0.4.3 (2026-10-12)

The second pass of the cold-agent loop: a seccomp profile with an unknown architecture is refused when written,
`a12s token create` is easy to capture in a script, and llms.txt states the traps the agents still fell into.

### Fixed
- **A seccomp profile with an unknown architecture is refused at the write** (`400 invalid`, naming the field), not
  accepted and then failed by runc at start, which left the agent computer in Error and `prompt()` waiting.
- **`a12s token create` prints the secret alone on stdout** (the header goes to stderr), so
  `TOKEN=$(a12s token create web --prefix web-)` captures exactly the secret.

## v0.4.2 (2026-10-12)

What nine apps written by fresh agents with only llms.txt tripped over: inbox holds and wake-friendly sleep in the
SDKs, connecting under a scoped token without a URL, Turn replays that end, warnings that reach SDK users, a check on
the anthropic credential when it is set, and an llms.txt that states the facts they had to guess.

### Added
- **`hold()` and `resume()` on an agent computer** in both SDKs (`POST …/inbox/hold|resume`): hold delivery while
  Messages still queue, then release it.
- **`sleep({ allowWake })` / `sleep(allow_wake=True)` and `wake({ clearInbox })` / `wake(clear_inbox=True)`** in both
  SDKs, as the CLI's `--allow-wake` and `--clear-inbox`. A plain `sleep()` still holds it asleep until `wake()`.
- **`Fleet.connect({ token })` / `a12s.Fleet(token=…)`** without a URL finds this machine's Fleet as before and uses
  the given token (a prefix or narrow token's secret) instead of the owner token.
- **A write's warnings reach SDK users**: `console.warn` in TypeScript, `warnings.warn(a12s.A12sWarning)` in Python.
- **A warning when an agent computer lacks its harness's credential** (claude-code: `anthropic`, codex: `openai`):
  a `credentials` list on one agent computer replaces the defaults' list, so `[stripe]` silently dropped
  `anthropic` and every Turn failed "Not logged in".
- **`a12s creds set anthropic` checks the value**: one unbroken `sk-ant-oat01-…` or `sk-ant-api03-…` string of about
  108 characters, else it refuses with the fix line (a setup token pasted in pieces used to surface later as a 401).
  `--no-check` skips the check.

### Fixed
- **A Turn replay ends**: `events({ turn })` / `events(turn=…)` stop after that Turn's `turn.finished`; they used to
  wait forever because the server keeps the stream open.
- **`a12s prompt … 2>&1`** no longer glues event lines onto the agent's text.
- **`a12s status`** says the orphaned-drive warning once, and CLI and API messages say "agent computer" instead of
  "Computer". `a12s up` prints `created a, b` instead of `created a, b (asleep: none)`.

## v0.4.1 (2026-10-12)

What a web console needs from the API, and the console itself: `a12s ui`. Live inbox and Service events on the
fleet-wide stream, before and after values in every change list, when credentials changed and tokens were last used,
a Controller port that survives restarts, and dry runs that fail exactly as the write would.

### Added
- **`inbox.changed` on `GET /v1/events`**: after any change to an agent computer's inbox (a Message sent, withdrawn,
  delivered or expired; a hold, resume or clear), its inbox summary (`pending`, `claimed`, `held`, `holdReason`,
  `usage`, …), a burst of changes as one event. In the SDKs' `fleet.events()` as `InboxChangedEvent`.
- **`service.phase` on `GET /v1/events`**: `{service, phase, health?, restarts, error?}` whenever a Service's phase,
  health or restart count changes. Its `agentComputer` is empty; tokens see it by the Service's name.
- **Before and after values in change lists**: every `update` in a write's `changes` (`?dryRun=true` included) and in
  `GET /v1/fleet`'s `drift` carries `diff: [{path, before?, after?}]`, one entry per changed leaf
  (`grants.send`, `instructions`, …). `fields` is unchanged.
- **`updatedAt` on credentials** (`GET /v1/credentials`, `PUT /v1/credentials/{name}`): when the value last changed.
  Kept in `credentials-updated.json` next to the values; values stored by an older Controller have none until set
  again.
- **`lastUsedAt` on tokens** (`GET /v1/tokens`): when the token last authenticated, recorded at most once a minute.
- **`a12s ui`** opens the web console (`npx github:coltonkirsten/a12s-console --fleet <fleet>`; `A12S_CONSOLE`
  overrides the package). Needs Node.js 20+.

### Fixed
- **`?dryRun=true` admits a write as the write does**: it resolves images and reads a Service's manifest from its
  image, so a dry run fails with the same `400 invalid` (e.g. `services.<s>.manifest.health: is required`) instead
  of listing a change the write then refuses. `a12s diff` may pull images it needs.

### Changed
- **The Controller's port is stable**: a new Controller container publishes a fixed loopback port (the one in
  `controller.json` if it is free), so `docker restart` keeps the port clients read from `controller.json`. A
  Controller created by an earlier release keeps its random port until `a12s up` replaces it on upgrade.

## v0.4.0 (2026-10-12)

The live Fleet: hold a `Fleet` and create, change and remove agent computers on it from code while it runs, with
credentials that survive restarts, prefix tokens that give an app its own lane, and one event stream for the whole
Fleet.

```python
fleet = a12s.Fleet()
hello = fleet.agent_computers.create("hello", instructions="Be helpful")
hello.prompt("hi")
hello.remove()
```

One unit is now an **agent computer** (`AgentComputer` in code), and the SDKs hand out objects you call methods on.
Agent computers and Services can be created, changed and removed while the Fleet runs.

### Breaking
- **HTTP routes are `/v1/agent-computers/...`** (were `/v1/computers/...`), with the same shapes. The old paths are
  still served, on the owner API and the mTLS listener, for one release. What to do: change the path in any client
  that calls the API directly; svckit already uses the new one.
- **The Fleet file key is `agent_computers:`** (was `computers:`). The old key is still read for one release and
  `a12s up` prints a hint. What to do: rename `computers:` to `agent_computers:` in `fleet.yaml`.
- **`PUT /v1/fleet` no longer takes `presets` or `seccomp`.** `a12s up` now sends a preset's files as `files` and a
  seccomp profile as JSON inside the Fleet, and `fleet.lock` no longer records presets or seccomp digests. An older
  CLI can't apply a Fleet with presets or seccomp profiles to this Controller. What to do: upgrade the CLI and the
  Controller together (they ship together), then run `a12s up`. The `presets:` and `seccomp:` entries of an
  existing `fleet.lock` are no longer used; you can delete them.
- **`GET /v1/status` lists agent computers under `agentComputers`** (Python: `agent_computers`). The old `computers`
  list is still sent for one release. What to do: read `agentComputers`.
- **The TypeScript type `Fleet` is now `FleetSpec`**: `Fleet` is the new class. What to do: rename the type in your
  code.
- **Name-first SDK calls are deprecated**: `connect()`, `A12sClient`, and in Python `a12s.connect()`,
  `a12s.connect_async()`, `Client` and `AsyncClient` (`client.prompt("hello", "…")`) still work for one release.
  What to do: `const fleet = await Fleet.connect()` / `fleet = a12s.Fleet()`, then
  `(await fleet.agentComputers.get("hello")).prompt("…")` / `fleet.agent_computers.get("hello").prompt("…")`.
- **The Controller stores credential values** in its state volume (`credentials.json`, mode 0600), so they survive
  a restart of Docker or the Controller; they were in memory only. Anyone who can read the file can already exec into
  an agent computer and read its secrets, and no API call returns a value. `a12s down` removes them with the state
  volume. What to do: nothing, or to keep them in memory only, run `a12s down`, then
  `a12s up --memory-only-credentials` (the flag applies when `up` starts a new Controller, which then deletes any
  stored file).
- **`a12s up` asks before undoing changes made through the API.** When agent computers or Services changed through
  the SDK or API since the last `a12s up` and this `up` would undo them, it lists the changes and asks
  `apply anyway? [y/N]`; without a terminal it fails. Its write is conditional on the Fleet version it read. What to
  do: in CI and scripts, pass `a12s up --force` to overwrite them as before, or run `a12s export` first to keep them
  in the file.

### Added
- **One event stream for the whole Fleet**: `GET /v1/events` (`?agentComputers=a,b` filters, `Last-Event-ID`
  resumes) carries every agent computer's events plus `fleet.changed {version, by, names}` and
  `agent_computer.phase {phase, error?}`, filtered to what the token may see. In both SDKs every `events()` and
  `turns()` of one Fleet object share this one connection.
- **Turn history**: the Controller keeps each agent computer's last 200 finished Turns, in the shape `turns()`
  yields: `GET /v1/agent-computers/{c}/turns?limit=20`, `agent.turns(limit=20)` / `agent.turns({ limit: 20 })`.
- **`egress.denied {host, port, reason}`** is its own event (it was a `diagnostic` "egress denied" line).
- **Normalized token usage**: `turn.finished` carries `tokens {input, output, cacheRead, cacheWrite}` for both
  harnesses, and a Turn has `tokens` (`TokenUsage`).
- Events and Turns carry `agentComputer` (`agent_computer` in Python); `computer` stays for one release.
- **The object SDK.** `Fleet.connect()` (TypeScript) and `a12s.Fleet()` / `a12s.AsyncFleet()` (Python) find the
  running Fleet like `connect()` did. `fleet.agentComputers` / `fleet.agent_computers` has `create(name, spec)`,
  `get(name)` (raises `NotFound`) and `list()`; an `AgentComputer` has `name`, `spec`, `effective` and `status`, and
  `reload`, `prompt`, `send`, `events`, `turns`, `update`, `sleep`, `wake`, `stop`, `inbox`, `message`, `withdraw`
  and `remove`. `fleet.services` (Service: `reload`, `update`, `sleep`, `wake`, `remove`), `fleet.tokens`
  (`create`, `list`, `remove`), `fleet.get()`, `fleet.replace(fn)` (a conditional whole-Fleet write that retries on
  a conflict), `fleet.events()`, `fleet.turns()` and `fleet.status()`. See [SDK](docs/sdk.md).
- **`instructions:`** on an agent computer (or in `defaults`): text written to its home as `CLAUDE.md` (Claude Code)
  or `AGENTS.md` (Codex). **`files:`** (`{path: text}`) adds more text files to its home, up to 1 MiB with the
  instructions; a file that starts with `#!` is executable. `home.preset` still works in the Fleet file: `a12s up`
  reads the directory and sends it as files (text only; symlinks and binary files are refused). `instructions` plus
  a preset `CLAUDE.md` is an error. In the SDK and the API, `security.seccomp` is the profile's JSON object.
- **Per-agent-computer and per-Service routes**: `GET /v1/agent-computers`, and `GET`, `PUT`, `PATCH` (JSON merge
  patch) and `DELETE` on `/v1/agent-computers/{c}` and `/v1/services/{s}`. `PUT` with `If-None-Match: *` only
  creates. These writes take no version: they touch only the named one, and the last write wins.
- **Fleet versions**: `GET /v1/fleet` returns the Fleet as written, its version (also the `ETag`) and `drift`, what
  changed through the API since the last `a12s up`. The version goes up by one with every change. `PUT /v1/fleet`
  takes `If-Match`. Every write takes `?dryRun=true` and answers with the new `version` and a `changes` list.
- Error codes `missing_field` (400: no harness or image in the spec or `defaults`), `missing_credential` (400: a
  credential with no value in the Controller), `exists` (409: a create-only write on a taken name),
  `version_mismatch` (412: a stale `If-Match`) and `in_use` (409: removing a credential that is still used).
- **Credentials from code**: `GET /v1/credentials` lists names with `set` and `usedBy`, never values;
  `PUT /v1/credentials/{name}` `{"value": "…"}` stores one and the agent computers and Services using it get it at
  once; `DELETE /v1/credentials/{name}` removes one (409 `in_use` while used). The SDKs wrap them as
  `fleet.credentials.set(name, value)`, `list()` (`CredentialInfo`: `name`, `set`, `usedBy` / `used_by`) and
  `remove(name)`. Owner token only. See [Credentials](docs/credentials.md#from-code-and-the-api).
- **`a12s diff`**: the Fleet file against the live Fleet, changing nothing (a dry run of `up`). It prints the change
  list and exits 2 when they differ, 0 when not; `--json` prints the changes.
- **`a12s export [--force]`**: writes the live Fleet back to `fleet.yaml`. Instructions up to 2000 characters with no
  files stay inline; longer ones and files go to `presets/<name>/` (`presets/defaults/` for `defaults`), seccomp
  profiles to `profiles/<name>.json`. It never writes credential values, and it refuses to overwrite a file that
  differs (comments in `fleet.yaml` are lost) or remove a stale preset file without `--force`. See
  [CLI](docs/cli.md#keeping-changes-made-from-code).
- **Prefix tokens**: `a12s token create bizapp --prefix biz-` (`fleet.tokens.create("bizapp", prefix="biz-")`,
  TypeScript `{ prefix: "biz-" }`, `POST /v1/tokens {"name", "prefix"}`) gives an app its own lane. It may do
  everything the owner can with agent computers, Services and credentials whose names start with `biz-`, and sees and
  does nothing outside: their `grants.send`, `uses` and drives (`defaults` included) must stay inside the prefix, it
  can't read or change the whole Fleet, drives or tokens, and anything outside answers the new error code
  `out_of_prefix` (403). Egress, image, seccomp and resources of its agent computers are its call. See
  [Security](docs/security.md#tokens).
- `a12s token create --agent-computers` is the new name of `--computers`, which still works.
- `a12s creds set <name>` also sets the value on the Fleet's running Controller, so agent computers using it get it
  at once; with no Controller running, `a12s up` sends it as before.
- `a12s up` sends every credential bound in `a12s.bindings.yaml`, not only the ones the file's agent computers name,
  so agent computers created from code can use any of them. Credentials merge into the Controller's store: `up` no
  longer drops ones it doesn't send. `a12s up --memory-only-credentials` keeps them out of the state volume.

### Changed
- `a12s up` replaces a running Controller of another release (its volumes, and the agent computers, stay), and a
  new Controller attaches itself to every agent computer's and Service's network. Upgrading is now: install the new
  release, set `a12s:` in `fleet.lock`, `a12s up`.
- A ComputerStatus's `seccomp` (and `a12s status`) shows the profile's `sha256:` digest instead of its path; `a12s up`
  prints `<name> runs with its own seccomp profile`.
- `a12s up` makes the running Fleet match the file, so it removes agent computers created through the SDK or API
  and undoes their changes; `GET /v1/fleet` lists them as `drift` until then.
- A Fleet may have no agent computers.
- `llms.txt` is enough on its own to build on a12s: credentials (and the wrapped setup-token paste), every Fleet
  field, what the agent sees, streaming with `prompt(..., { onEvent })`, a hand-off recipe, custom images, Services
  with a complete svckit example and manifest, and a gateway page. The README links its raw URL for coding agents.

## v0.3.0 (2026-10-12)

a12s is now its core: the Controller, computerd, the slim image, the harness bundles, svckit, the TypeScript and
Python SDKs, and the gateway. The release publishes the `slim`, `controller`, `bundle-claude-code` and `bundle-codex`
images.

### Breaking
- **The `browser` image is gone**, and computerd no longer wires the Playwright MCP server (or `NODE_PATH`).
  `image: browser` now fails validation; `A12S_IMAGE_TIER` is `slim` or `custom`. What to do: build the cookbook's
  [browser-agent](https://github.com/coltonkirsten/a12s-cookbook/tree/main/examples/browser-agent) image (FROM slim
  with Chromium), copy its `profiles/chromium.json` next to your `fleet.yaml`, replace `image: browser` with your
  image and add `security: { seccomp: ./profiles/chromium.json }`. See
  [Images: A browser in your image](docs/images.md#a-browser-in-your-image).
- **The reference Services are gone**: `svc-cron`, `svc-webhook`, `svc-mcp` and `svc-mcp-postgres`, with
  `examples/services`, `make services` and `make image-browser`. The Services feature itself is unchanged. What to do:
  keep using the published `ghcr.io/coltonkirsten/a12s/svc-*:0.2.1` images by pinning that tag, build them from the
  [v0.2.1 source](https://github.com/coltonkirsten/a12s/tree/v0.2.1/services), or write your own with svckit
  ([Services: Write a Service with svckit](docs/services.md#write-a-service-with-svckit)).
- **`a12s init --with db,cron` is gone.** `a12s init` always scaffolds one Computer, `hello`, on `image: slim`. What
  to do: add Services to `fleet.yaml` yourself.
- `scripts/check.sh` steps are `go computerd sdk python slim controller bundles` (`services` and `tiers` are gone).

### Added
- **`security.seccomp`** on a Computer (or in `defaults`): a seccomp profile in Docker's JSON format, relative to the
  Fleet file, that replaces Docker's default profile for that Computer. Only the Fleet's owner sets it, never an
  image; it changes only the seccomp filter (capabilities and `no-new-privileges` stay). `a12s up` checks the profile
  before sending anything, pins its digest in `fleet.lock` under `seccomp:`, and restages the Computer when it
  changes; `a12s up` and `a12s status` name every Computer that runs with one. See
  [Security: Seccomp](docs/security.md#seccomp).
- **`a12s gateway -- npm run dev`**: run your dev server (Vite, Next.js, …) behind the gateway. It starts the command,
  finds its port in the URL it prints (or `--dev-port`), and forwards everything except `/v1/` and `/a12s.js` to it,
  websockets included, so the page and the API share one origin and hot reload works. `--dev <url>` proxies a dev
  server that is already running. See [Recipes: A web app](docs/recipes.md#a-web-app-react-vite-any-dev-server).
- `llms.txt` lists every HTTP route (with the action a scoped token needs) and every SDK client method, generated
  from the code and checked in CI, so it can no longer drift.

### Changed
- Examples now live in [a12s-cookbook](https://github.com/coltonkirsten/a12s-cookbook), one folder per example in
  TypeScript and Python. The `a12s-starter` template is archived. What to do: start from a cookbook example or
  `a12s init`.
- **`a12s gateway` is locked with a key made for each run**, like Jupyter: it opens `http://127.0.0.1:7012/?key=…` in
  your browser (`--no-open` only prints it), trades the key for an HttpOnly cookie and drops it from the URL. `/v1/`
  and `/a12s.js` need the cookie or `Authorization: Bearer <key>`, so other users and programs on the machine can't
  use the Fleet through it. What to do: open the printed link instead of typing `http://127.0.0.1:7012`.
- Each gateway has its own scoped token, `gateway-<port>` (was `gateway`, shared), revoked when it stops; tokens left
  by a killed gateway are removed when the next one starts, and a running gateway's token is never touched. Several
  gateways can serve one Fleet. What to do: Messages from the page now arrive `via` `token:gateway-<port>`; match on
  that prefix if your code checked for `token:gateway`.
- `a12s gateway --addr host:port` listens on another loopback address; a non-loopback `--addr` (`0.0.0.0`) or a
  request for a non-loopback host name is refused with how to serve other people instead
  ([SDK: Serve other people](docs/sdk.md#serve-other-people)).
- `a12s gateway` prints what the page may do in one line when it starts (its token's actions and Computers).
- `brew install coltonkirsten/tap/a12s` no longer needs `HOMEBREW_GITHUB_API_TOKEN`: the repo is public, so the formula
  downloads the release assets directly.

## v0.2.1 (2026-10-12)

### Changed
- **The npm package is `@coltonkirsten/a12s`** (npm refused the bare name `a12s`). `npm install @coltonkirsten/a12s` and
  `import { connect } from "@coltonkirsten/a12s"`; the API is unchanged. 0.2.0 is on npm under this name. The Python
  package stays `a12s` on PyPI. The release's npm tarball is now `coltonkirsten-a12s-<version>.tgz`.
- Releases publish `@coltonkirsten/a12s` to npm and `a12s` to PyPI from GitHub Actions with trusted publishing (no
  stored tokens); 0.2.1 is the first.

## v0.2.0 (2026-10-12)

### Added
- SDK Turns carry `tools`: every tool call with its input and output, so what an agent `send`s to another Computer
  is on the Turn.
- Docs built for skimming: a 5-minute quickstart, [Concepts](docs/concepts.md) led by the five ideas you need,
  [Recipes](docs/recipes.md), and [`llms.txt`](llms.txt), the whole surface on one page for coding agents.
- `a12s --version` (and `-v`).
- **Python SDK (`pip install a12s`)**: the same client as TypeScript, sync (`a12s.connect()`) and async
  (`a12s.connect_async()`), typed dataclasses, `with client.turns([...]) as turns:` to watch before you send.
  Shares the Go-rendered API fixtures, so a change to the API's JSON fails its tests until the SDK follows.
- Releases attach both SDK packages (`a12s-<version>.tgz`, `a12s-<version>-py3-none-any.whl`) to the GitHub release.
- **SDK (`npm install a12s`)**: `connect()` finds the running Fleet (nearest `fleet.yaml`, `$A12S_FLEET`, or the only
  running Fleet); `events()` and the new `turns()` follow one Computer, several, or the whole Fleet, merged, with
  `ready()` so you can start watching before you send; events are a typed union (narrow on `type`).
- **`a12s gateway`**: a web page can reach the Fleet. It serves your `--web` directory (or a built-in live
  dashboard), the browser SDK at `/a12s.js`, and the API under a scoped token the page never sees.
- `docs/sdk.md` and `docs/api.md` (the HTTP API: routes, JSON, SSE framing, error codes).

### Changed
- Claude Code Computers load the `a12s` system tools (`send`, `lock`, `release`, `restart_harness`) up front instead
  of behind tool search, so an agent no longer spends a ToolSearch step before every `send`.
- **The npm package is `a12s`** (was `@coltonkirsten/a12s-client`). `import { connect } from "a12s"` works in Node
  and browsers; the `/node` subpath and `fromFleet` are gone (use `connect({ fleet })`). `enqueue`,
  `messageEvents` and `waitForTurn` are gone (use `send` and `prompt`). `prompt()` resolves with a `Turn` (`id`,
  `text`, `status`, …) plus `message`; the Turn id moved from `turn` to `id`. `stop(name)` stops the active Turn.
  New: `sleep(name)`, `wake(name)`.
- SDK event streams resume from the Controller's `head` after a reconnect, so a drop before the first event no
  longer loses events.
- **Presets follow their directory.** Edit a `home.preset` (say `presets/lead/CLAUDE.md`) and run `a12s up`: the
  files are copied onto the home drive at the Computer's next idle boundary and the session continues. `fleet.lock`
  updates the preset's entry instead of refusing the change. Before, a preset was copied once onto an empty home,
  and changing it needed a lock edit plus `a12s down --purge`.
  *Upgrade note:* every Computer with a preset restarts once at its next idle boundary after the upgrade.
