# Changelog

Breaking changes are allowed before 1.0 when they make a12s simpler; each one is listed here with what to do.

## Unreleased

### Added
- **SDK (`npm install a12s`)**: `connect()` finds the running Fleet (nearest `fleet.yaml`, `$A12S_FLEET`, or the only
  running Fleet); `events()` and the new `turns()` follow one Computer, several, or the whole Fleet, merged, with
  `ready()` so you can start watching before you send; events are a typed union (narrow on `type`).
- **`a12s gateway`**: a web page can reach the Fleet. It serves your `--web` directory (or a built-in live
  dashboard), the browser SDK at `/a12s.js`, and the API under a scoped token the page never sees.
- `docs/sdk.md` and `docs/api.md` (the HTTP API: routes, JSON, SSE framing, error codes).

### Changed
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
