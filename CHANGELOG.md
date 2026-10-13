# Changelog

Breaking changes are allowed before 1.0 when they make a12s simpler; each one is listed here with what to do.

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
  [CLI](docs/cli.md#changes-made-through-the-api).
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
