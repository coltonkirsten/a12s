# Architecture

a12s is one Go binary (`a12s`) that is both the CLI and, inside its own container, the Controller, plus a TypeScript
daemon (computerd) that runs inside every Computer.

```text
host                          a12s-<fleet>-controller (bridge network a12s-<fleet>)
┌──────────┐  loopback +      ┌───────────────────────────────────────────────┐
│ a12s CLI │──bearer token───▶│ owner API        reconciler      fleet.db     │
└──────────┘                  │ mTLS listener    CA              egress proxy │
                              └───────┬───────────────────────────────┬───────┘
                         mTLS :7443   │                               │ :3128 on every internal network
                ┌─────────────────────▼──────┐          ┌─────────────▼──────────┐
                │ Computer a12s-<fleet>-scout │          │ Service a12s-<fleet>-db │
                │ computerd ─▶ harness        │          │ a12s-init ─▶ image      │
                │ home drive, work drives     │          │ state drive             │
                └─────────────────────────────┘          └────────────────────────┘
                  one isolated internal network per Computer and per Service
```

## Controller

The Controller runs in a container started by `a12s up` (`ghcr.io/coltonkirsten/a12s/controller:<version>` for a release, `a12s/controller:dev` for a source build). It:

- **Reconciles** the Fleet against what exists, through the Driver interface. Every mutation carries a
  generation; stale writes are rejected and success is read back, never assumed. Changes to a busy Computer are
  staged and applied at the next idle boundary.
- **Holds power state**, the idle clocks and the power policy.
- **Owns the inboxes** in `fleet.db`, a SQLite database (WAL, `synchronous=FULL`) on the `a12s-<fleet>-data`
  volume. It also keeps holds, scoped tokens, the certificate registry and power-policy state, so they survive a
  plain `down`/`up`.
- **Is the CA** and issues a certificate per Computer or Service start. The CA key lives on the Controller's state
  volume.
- **Runs the egress proxy** on port 3128 of every internal network.
- **Proxies event streams** from computerd to API clients.

### Owner API

Listens on the bridge interface and is published only on `127.0.0.1:<port>`; the port and a random bearer token are
in `~/.a12s/<fleet>/controller.json` (0600). Scoped tokens authenticate the same way. All responses are JSON; errors
are `{code, message, fix?, details?}` with a stable `code`. The main routes:

| Route | Purpose |
|---|---|
| `PUT /v1/fleet` | Apply a Fleet, lock and credentials |
| `GET /v1/status` | Computers, Services, inboxes, power |
| `POST /v1/computers/{c}/inbox` | Enqueue (`GET`, `DELETE …/inbox/{id}`, `…/inbox/{id}/events`, `…/inbox/clear`, `…/inbox/hold`, `…/inbox/resume`) |
| `GET /v1/computers/{c}/events` | The Computer's event stream (SSE, `Last-Event-ID`, `?turn=`) |
| `POST /v1/computers/{c}/turns/{id}/stop` | Stop a Turn |
| `POST /v1/computers/{c}/power` | Sleep or wake |
| `POST /v1/apply` | Apply staged changes with `interrupt` and `grace` |
| `GET /v1/drives`, `DELETE /v1/drives/{d}` | Drives |
| `/v1/tokens` | Scoped tokens |
| `/v1/services/…` | Service list, status, logs, power, login |

### mTLS listener

Computers and Services call the Controller with their certificates. Only the live certificate for a running
container is accepted. Services and Computers with `grants.send` may enqueue and read or withdraw their own Messages;
Computers also take leases (for `lock`/`release`) and write diagnostics. Nothing else exists on this listener.

## Docker driver

| Thing | Docker resource |
|---|---|
| Computer `C` | Container and internal network `a12s-<fleet>-C`; home drive volume `a12s-<fleet>-home-C` |
| Work drive `D` | Volume `a12s-<fleet>-drive-D` |
| Harness bundles | Read-only cache volume `a12s-<fleet>-bundles` |
| Controller | Container `a12s-<fleet>-controller`, its state volume, bridge network `a12s-<fleet>` |
| Inboxes | Volume `a12s-<fleet>-data` |
| Service `S` | Container and network `a12s-<fleet>-S`; state drive `S-state`; `a12s-<fleet>-S-publish` forwarder when it publishes a port |

Resources carry `a12s.*` labels. Internal networks are small subnets from a pool (default `10.213.0.0/16`, a /29
each), so many Computers don't exhaust Docker's default pool. There are no host ports. Containers run with Docker's
`--init` so orphaned harness children are reaped.

## Inside a Computer

- **Mounts:** the home drive at `/home` (`/home/agent` for the agent; `/home/.a12s` root-only for the journal and
  session record), work drives at their `at:`, the bundle cache read-only at `/opt/a12s/bundles`, tmpfs
  `/run/a12s` (secrets, TLS, computerd's socket) and `/tmp`.
- **computerd** (`/opt/a12s/computerd`) waits for its TLS files, listens with mTLS on `:7443`, keeps the journal
  (`/home/.a12s/journal.db`, SQLite) and runs the harness as uid 1000.
- **Environment** set by the driver includes `A12S_COMPUTER`, `A12S_FLEET`, `A12S_HARNESS_KIND`,
  `A12S_HARNESS_VERSION`, `A12S_MODEL`, `A12S_AUTONOMY`, `A12S_BUNDLE`, `A12S_CREDENTIALS`, `A12S_DRIVES`,
  `A12S_PROXY` and `A12S_CONTROLLER`, plus `A12S_SERVICE_<SVC>_<PORT>` for each used Service port.

### computerd API

| Call | Shape |
|---|---|
| `POST /v1/turns` | `{requestId, session: current\|new\|<id>, prompt, cwd, inbox?}` → `{turnId, sessionId}` once durably accepted. One Turn at a time (`409` when busy). A repeated `requestId` with the same body returns the original Turn. |
| `GET /v1/events` | One SSE stream per Computer; `Last-Event-ID` and `?turn=`. An expired cursor gets an explicit `gap`. |
| `GET /v1/turns/{id}` | A Turn's summary: dispatched, resume failed, finished, error. |
| `POST /v1/turns/{id}/stop` | Idempotent stop. |
| `GET /v1/status` | Phase, session, active Turn, harness version, credential generation, owed continuation. |
| `POST /v1/diagnostics`, `POST /v1/journal/inbox` | The Controller journals diagnostics and ended Messages. |
| `POST /v1/continuation/cancel` | Drop an owed `restart_harness` continuation (used by `inbox resume --new`). |

### Adapters

- **Claude Code:** the Claude Agent SDK (`query()`) driving the bundle's `claude` binary as uid 1000, with an explicit
  session id and `resume`. User, project and local settings load, so the agent's own skills and MCP config work.
  Stop interrupts, then sends SIGTERM after 10 s and SIGKILL after 5 s more. One process per Turn.
- **Codex:** `codex app-server` over JSON-RPC on stdio, one long-lived child per Computer: `thread/start` or
  `thread/resume`, `turn/start`, `turn/interrupt`. Every server request (approvals, user input, elicitation) is
  answered according to `autonomy`.
- Both get the `a12s` system tools MCP server; each used Service's MCP port is registered as `a12s-<svc>` through a
  stdio-to-HTTP bridge.

A Turn counts as **dispatched** when the adapter has evidence the harness received the prompt (Claude Code's echo of
the user message; Codex's `turn/start` response). The inbox uses this to decide whether a Batch was delivered.

## Inside a Service

A Service has no computerd. Its entrypoint is `a12s service-init` from a read-only `a12s-<fleet>-init` volume, which
prepares the state mount and tmpfs paths, waits for injected secrets, exports `config.env`, starts any TCP
forwarders, then drops to the manifest uid with no capabilities and execs the image's own entrypoint. See
[Services](services.md).
