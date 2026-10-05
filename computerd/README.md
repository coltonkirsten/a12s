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
