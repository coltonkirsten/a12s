# PLAN — a12s Layer 1

Spec: `docs/a12s-layer1-design.md` (sections cited as §N) plus
`docs/adapter-check-2026-10-05.md` (adapter changes). Scope: Docker driver only;
`slim` and `browser` image tiers; Claude Code and Codex harnesses.

## Layout

```text
cmd/a12s/                 the CLI and, as `a12s controller`, the Controller (one Go binary)
internal/fleet/           §3  Fleet file: parse, defaults merge, validation, fleet.lock
internal/driver/          §4  Driver interface + types (Docker now, Kubernetes later)
internal/driver/docker/   §4  Docker driver: containers, internal networks, volumes, bundles
internal/controller/      §4,§6,§7  reconciler, state machine, CA, credential inject, egress proxy, API
internal/api/             CLI <-> Controller API types (JSON)
internal/computerd/       Go client for computerd's HTTP+SSE contract (§5)
computerd/                §5  TypeScript daemon: server, journal, adapters, system tools MCP
images/slim, images/browser          §6  image tiers
images/bundles/claude-code, codex    §5  HarnessBundle images (pinned, verified downloads)
examples/lab/             §3  the exit-gate Fleet, bindings template, presets
.github/workflows/ci.yml  build, vet/lint, unit tests (no secrets)
```

## Milestones

| # | Milestone | Spec | Exit gate |
|---|---|---|---|
| M1 | Fleet parsing/validation/lock; Driver interface; Docker driver | §3, §4 | 1, 5 |
| M2 | computerd: journal, HTTP+SSE, Claude + Codex adapters, system tools | §5 | 3, 4, 6 |
| M3 | Images: slim, browser, HarnessBundles | §5, §6 | 1, 8 |
| M4 | Controller: reconcile, CA/mTLS, inject, egress proxy, API; CLI | §4, §6, §7, App. A | 2, 5, 7, 8 |
| M5 | Integration, Astra reviews, exit gate on mac-mini Docker | §8 | 1–8 |

## Contracts (the parts two packages must agree on)

### Naming on Docker
Fleet `F`, Computer `C`, Drive `D`. Labels: `a12s.fleet`, `a12s.computer`,
`a12s.generation`, `a12s.role` (`controller` | `computer`). Names:
`a12s-F-C` (container, internal network), `a12s-F-home-C` (home drive),
`a12s-F-drive-D` (work drive), `a12s-F-bundles` (read-only bundle cache),
`a12s-F-controller` (controller container + its state volume + bridge network
`a12s-F`). Internal networks come from a configured pool (default `10.213.0.0/16`, /29 each).

### Container (both tiers)
- Base `node:24-bookworm-slim` + ca-certificates, git, ripgrep, python3, curl, util-linux.
  `browser` adds Debian `chromium`, fonts, `@playwright/mcp` at `/opt/a12s/playwright-mcp`,
  and ships `images/browser/seccomp.json` (default profile + user namespaces so Chromium keeps its own sandbox).
- computerd at `/opt/a12s/computerd` (`dist/` + `node_modules`). Entrypoint `node /opt/a12s/computerd/dist/main.js`, runs as root with
  `--cap-drop ALL --cap-add CHOWN,DAC_OVERRIDE,FOWNER,SETUID,SETGID,KILL`, `no-new-privileges`, read-only rootfs.
  It spawns the harness as uid/gid 1000 (`agent`). Nothing else runs as root.
- Mounts: home drive at `/home` (computerd creates `/home/agent` 1000:1000 and `/home/.a12s` root 0700 — journal,
  session record; the agent can neither open nor rename it); work drives at their `at:`; bundle cache ro at
  `/opt/a12s/bundles`; tmpfs `/run/a12s` (root 0711; `secrets/` root 0711 with agent-owned 0600 files, `tls/` root 0700, `sock/` 0755);
  tmpfs `/tmp`; `--init` so orphaned harness children are reaped. No host mounts, no Docker socket, no ports.
  `/run/a12s` is 0711 (the agent must traverse into `secrets/` and `sock/`, and still cannot open `tls/`).
- Env set by the driver: `A12S_COMPUTER`, `A12S_FLEET`, `A12S_GENERATION`, `A12S_HARNESS_KIND` (`claude-code`|`codex`),
  `A12S_HARNESS_VERSION`, `A12S_MODEL` (optional), `A12S_AUTONOMY` (`full`|`restricted`), `A12S_IMAGE_TIER` (`slim`|`browser`|`custom`),
  `A12S_BUNDLE=/opt/a12s/bundles/<kind>-<version>-<imageID[:12]>`, `A12S_CREDENTIALS=anthropic,openai,…`,
  `A12S_DRIVES=<at>:rw,<at>:ro,…` (computerd chowns an empty, root-owned rw mount root to 1000:1000),
  `A12S_PROXY=http://a12s-F-controller:3128`, `A12S_CONTROLLER=https://a12s-F-controller:7443`.

### HarnessBundle image
OCI image whose filesystem is `/bundle/{bundle.json, bin/<binary>, …}` (Codex ships its release package layout beside `bin/codex`). `bundle.json`:
`{"kind":"claude-code","version":"2.1.280","arch":"arm64","bin":"bin/claude","signInHosts":["api.anthropic.com","platform.claude.com", …]}`.
The Controller extracts it into the bundle cache as `/opt/a12s/bundles/<kind>-<version>-<imageID[:12]>/` (keyed by image identity, so a re-tagged image never reuses old files) and adds
`signInHosts` to that Computer's egress allowlist. Default image refs: `a12s/bundle-<kind>:<version>`;
tiers `a12s/slim:dev`, `a12s/browser:dev`; controller `a12s/controller:dev`. `fleet.lock` pins image IDs:
```yaml
images:  { slim: "sha256:…", browser: "sha256:…" }
bundles: { "claude-code@2.1.280": "sha256:…", "codex@0.154.0": "sha256:…" }
presets: { "./presets/scout": "sha256:…" }   # content hash of the directory
```
`a12s up` resolves and appends missing entries; it never changes an existing one.
`.github/workflows/bundles.yml` builds each new upstream release daily (amd64+arm64), runs `images/bundles/conformance.sh`, and publishes
`ghcr.io/coltonkirsten/a12s/bundle-<kind>:<version>` (private); the local refs above stay the defaults, and `fleet.lock` pins whichever ref by image ID.

### Secrets and TLS (§6)
Controller holds credential material in memory only (the CLI reads the bindings file on the host and sends it with
`up` and `wake`). Injection: `docker exec -i` with the bytes on stdin into `/run/a12s/secrets/<name>` (1000:1000, 0600 — the harnesses rewrite their
credential file when they refresh a subscription token; after every turn computerd restores the symlink if a harness replaced
it with a regular file, moving the fresh content back onto tmpfs so no token stays on the home drive),
`/run/a12s/tls/{ca.crt,cert.pem,key.pem}` (root 0600), then `/run/a12s/secrets/.generation` (an integer) last.
computerd waits for the TLS files before listening, and reports `.generation` as `credentialGeneration`.
Wiring by name inside computerd: `anthropic` — JSON → symlink `$CLAUDE_CONFIG_DIR/.credentials.json` to it, raw key →
`ANTHROPIC_API_KEY`; `openai` — JSON → symlink `$CODEX_HOME/auth.json`, raw key → `OPENAI_API_KEY`; `github` → `GH_TOKEN`
and `GITHUB_TOKEN`. Other names: file only. `CLAUDE_CONFIG_DIR=/home/agent/.claude`, `CODEX_HOME=/home/agent/.codex`.
The CA key lives on the controller's state volume; a server cert per Computer generation (SAN = computer name) and one
controller client cert. computerd requires the client cert; the Controller verifies the server name.

### computerd HTTP (mTLS, :7443) — §5 verbatim, plus one call
`POST /v1/turns`, `GET /v1/events` (SSE, `Last-Event-ID`, `?turn=`), `POST /v1/turns/{id}/stop`, `GET /v1/status`,
and `POST /v1/diagnostics {message, data}` so the Controller can journal a denied egress as a `diagnostic`.
Event envelope `{v:1, computer, session, turn, seq, ts, type, data}`; `seq` is the SSE id. Types as in §5
plus `harness.restarted`. An unknown/future cursor gets `event: gap`.
Journal: SQLite (`node:sqlite`) at `/home/.a12s/journal.db`: `events` and `turns(request_id, turn_id, body_hash)`.
Session record `/home/.a12s/session.json` `{sessionId, harness, cwd}`; cwd is `/home/agent` (stable across generations).

### Adapters
- Claude: `@anthropic-ai/claude-agent-sdk` `query()` with `pathToClaudeCodeExecutable=$A12S_BUNDLE/bin/claude`,
  `spawnClaudeCodeProcess` (uid/gid 1000), `resume`/`sessionId`, `settingSources: ['user','project','local']`,
  `mcpServers` (`a12s` system tools; `playwright` on the browser tier: `node /opt/a12s/playwright-mcp/node_modules/@playwright/mcp/cli.js
  --config /opt/a12s/playwright-mcp/config.json --proxy-server $A12S_PROXY`), `strictMcpConfig: false` so MCP servers the agent
  writes to its home drive load after `restart_harness` (a cloned repo's `.mcp.json` therefore also loads: threat-model note), `includePartialMessages`,
  `full` → `bypassPermissions` + `allowDangerouslySkipPermissions`; `restricted` → `dontAsk` + `permissionPrompts: 'none'`,
  each denial → `permission` event. Stop: `interrupt()`, then SIGTERM after 10s, then SIGKILL after 5s. One process per turn.
- Codex: `$A12S_BUNDLE/bin/codex app-server` on stdio, one child per Computer; `initialize`/`initialized`,
  `thread/start` or `thread/resume`, `turn/start`, `turn/interrupt`; `item/agentMessage/delta` → `text.delta`;
  `item/started|completed` → `tool.*`; `turn/completed` → `turn.finished`. Every server request
  (`item/*/requestApproval`, `item/tool/requestUserInput`, `mcpServer/elicitation/request`, legacy approvals) is answered:
  `full` = `approvalPolicy: never` + `danger-full-access`; `restricted` = `on-request` + `workspace-write`, auto-decline + `permission` event.
  MCP servers via `-c mcp_servers.<name>.command=…`.
- Wake precheck: Codex `thread/resume`; Claude session file exists. Failure → `resume.failed`, status `resumePending` stays.
- System tools MCP (`computerd/dist/systemtools.js`, stdio, spawned by the harness as the agent; talks to computerd over
  `/run/a12s/sock/computerd.sock`): `restart_harness`, `lock`, `release`. Locks are Controller leases (`POST/DELETE /v1/leases` on the
  Controller's mTLS listener), TTL 60s, renewed by computerd while held.

### Controller API (loopback, bearer token)
Listens on the bridge interface only; published `127.0.0.1:<port>`; token and port in `~/.a12s/F/controller.json` (0600).
`PUT /v1/fleet {fleet, lock, credentials, asleep}` → `{created, updated, staged, removed, lock}`; `GET /v1/status`;
`POST /v1/computers/{c}/turns`; `GET /v1/computers/{c}/events` (proxied SSE); `POST /v1/computers/{c}/turns/{id}/stop`;
`POST /v1/computers/{c}/power {state, credentials}`; `POST /v1/apply {interrupt, grace}`; `POST /v1/computers/{c}/shell` (audit, returns container);
`GET /v1/drives`; `DELETE /v1/drives/{d}`. Busy → 409, Asleep → 503, passed through from computerd.

### Egress proxy (§6)
HTTP proxy on `:3128` of every internal network. The Computer is identified by the listener's local address (one
Computer per network). CONNECT to allowed host:443 (or any port for `*`), plain `http://` only to allowed hosts. Resolves on
the proxy side, rejects private/link-local/metadata results, connects to the validated address. Denied → 403 and
`POST /v1/diagnostics` to that computerd. Internal networks use `gateway_mode_ipv4=isolated`; there is no other route.

### Decisions taken during implementation
- computerd does every filesystem step under `/home/agent` (config dirs, credential symlinks, reclaim) as uid 1000 (`dist/agentfs.js`) and refuses symlinks in place of directories it manages; it reads only the credential names in `A12S_CREDENTIALS`, with `O_NOFOLLOW`.
- Driver is bound to one Fleet at construction; `EnsureBundle` returns the parsed `bundle.json` (the Controller needs `signInHosts`).
- The preset is applied at stage time into the stopped container's `/home/agent`, only when that directory does not exist yet.
- A prompt whose resume fails is accepted (200) and journals `turn.started` → `resume.failed` → `turn.finished failed`; the session id is kept.
- SSE: only `gap` and `head` (`{seq}` after the journal replay, before live events) carry an `event:` line; a `?turn=` stream without a cursor replays that turn from its start.
- A Controller `diagnostic` is journaled under the active turn with `source: "controller"`.

## Spec deviations
- **Local caller auth**: Docker Desktop cannot share a Unix socket between the macOS host and the Controller container,
  so the Controller listens on `127.0.0.1:<port>` with a random token in an owner-only file. Same trust boundary, different pipe.
- **Shell**: `a12s shell` asks the Controller to audit the request (journal `lifecycle`), then runs `docker exec` against the
  named container directly; a TTY relay through the Controller is deferred with the Kubernetes driver.
- **Drive size on Docker**: named volumes cannot enforce a quota; `size` is recorded and `capabilities()` reports `driveQuota: false`.
- **Secret file mode** is 0600, not 0400 (see Secrets and TLS): a 0400 file breaks subscription token refresh.
- **Token refresh is one-way**: a token refreshed inside a Computer is not written back to the host bindings; re-run `a12s up` with a fresh binding to rotate.
- **Controller restart** forgets credentials (they are never on disk); `a12s status` says so and `a12s up` re-sends them.
