# Changelog

Breaking changes are allowed before 1.0 when they make a12s simpler; each one is listed here with what to do.

## Unreleased

### Added
- **`a12s gateway -- npm run dev`**: run your dev server (Vite, Next.js, …) behind the gateway. It starts the command,
  finds its port in the URL it prints (or `--dev-port`), and forwards everything except `/v1/` and `/a12s.js` to it,
  websockets included, so the page and the API share one origin and hot reload works. `--dev <url>` proxies a dev
  server that is already running. See [Recipes: A web app](docs/recipes.md#a-web-app-react-vite-any-dev-server).
- `llms.txt` lists every HTTP route (with the action a scoped token needs) and every SDK client method, generated
  from the code and checked in CI, so it can no longer drift.

### Changed
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
