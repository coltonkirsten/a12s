# computerd

The daemon inside every Computer (§5): journal, mTLS HTTP+SSE on :7443, Claude Code and Codex adapters, and the
`a12s` system tools MCP server. `npm run build` → `dist/main.js`, `dist/systemtools.js`; `npm test`; `npm run lint`.

## Dev mode (on a host, not root)
Not root ⇒ no chown/uid switch, and `A12S_HOME_ROOT` (default `/home`), `A12S_RUN_DIR` (default `/run/a12s`) and
`A12S_PORT` (default 7443) may point elsewhere. Put `ca.crt`, `cert.pem`, `key.pem` in `$A12S_RUN_DIR/tls/`, secrets in
`$A12S_RUN_DIR/secrets/<name>`, a bundle dir with `bin/claude` or `bin/codex`, then
`A12S_COMPUTER=c A12S_HARNESS_KIND=codex A12S_BUNDLE=… A12S_HOME_ROOT=… A12S_RUN_DIR=… node dist/main.js`.
Real-harness smoke run (temp dirs, throwaway openssl CA, lab credentials copied in and deleted after):
`npm run build && node scripts/smoke.mjs claude-code` and `node scripts/smoke.mjs codex`
(override `CLAUDE_BIN`, `CODEX_BIN`, `A12S_LAB_CREDS`). Steps: pong, resume, stop, restart_harness, restart+resume.

## Credential files
- Recommended Claude credential for Computers: a `claude setup-token` token (`sk-ant-oat…`) as the raw `anthropic`
  secret. computerd passes it as `CLAUDE_CODE_OAUTH_TOKEN`: no credential file, no refresh, nothing on any disk.
  `sk-ant-api…` becomes `ANTHROPIC_API_KEY`; a JSON `anthropic` secret is symlinked as `$CLAUDE_CONFIG_DIR/.credentials.json`.
- Refresh writes are in place, so they follow the symlink onto tmpfs: Codex 0.154.0 `codex-rs/login/src/auth/storage.rs`
  `save()` opens `auth.json` with `OpenOptions` `truncate(true).write(true).create(true)` (mode 0600) and writes through
  the path; Claude Code writes its credentials file with Node's `fs.writeFileSync`, which does the same.
- Reclaim (fs.watch plus after-turn, start and stop) exists only for a harness that ever switches to atomic replacement:
  it moves a newer on-disk file onto the tmpfs secret (temp + rename on tmpfs, dropped if an injection lands meanwhile)
  and restores the symlink. Until it runs (about a second) such a token would sit on the home drive.
