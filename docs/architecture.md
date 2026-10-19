# Architecture

a12s is one Go binary (`a12s`) that is both the CLI and, inside its own container, the Controller, plus a TypeScript
daemon (computerd) that runs inside every agent computer.

```text
host                          a12s-<fleet>-controller (bridge network a12s-<fleet>)
┌──────────┐  loopback +      ┌───────────────────────────────────────────────┐
│ a12s CLI │──bearer token───▶│ owner API        reconciler      fleet.db     │
└──────────┘                  │ mTLS listener    CA              egress proxy │
                              └───────┬───────────────────────────────┬───────┘
                         mTLS :7443   │                               │ :3128 on every internal network
                ┌─────────────────────▼──────┐          ┌─────────────▼──────────┐
                │ agent computer              │          │ Service a12s-<fleet>-db │
                │ a12s-<fleet>-scout          │          │                         │
                │ computerd ─▶ harness        │          │ a12s-init ─▶ image      │
                │ home drive                  │          │ state drive             │
                └─────────────────────────────┘          └────────────────────────┘
                  one isolated internal network per agent computer and per Service
```

## Controller

The Controller runs in a container started by `a12s up` (`ghcr.io/coltonkirsten/a12s/controller:<version>` for a release, `a12s/controller:dev` for a source build). It:

- **Holds the Fleet** as written (defaults and each agent computer before they are merged) with a version that goes
  up by one per change. The live Fleet is the source of truth; `a12s up` makes it match the file. Writes take an exclusive lock: a whole-Fleet write may name the version it read, a write to one agent computer or
  Service touches only that one.
- **Reconciles** the Fleet against what exists, through the Driver interface. Every mutation carries a
  generation; stale writes are rejected and success is read back, never assumed. Changes to a busy agent computer
  are staged and applied at the next idle boundary.
- **Holds power state**, the idle clocks and the power policy.
- **Owns the inboxes** in `fleet.db`, a SQLite database (WAL, `synchronous=FULL`) on the `a12s-<fleet>-data`
  volume. It also keeps holds, scoped tokens, the certificate registry and power-policy state, so they survive a
  plain `down`/`up`.
- **Holds the credentials** by name in `credentials.json` (0600) on its state volume and injects them into
  containers. It never returns a value.
- **Is the CA** and issues a certificate per agent computer or Service start. The CA key lives on the Controller's state
  volume.
- **Runs the egress proxy** on port 3128 of every internal network.
- **Proxies event streams** from computerd to API clients.

### Owner API

Listens on the bridge interface and is published only on `127.0.0.1:<port>`; the port and a random bearer token are
in `~/.a12s/<fleet>/controller.json` (0600). Scoped tokens authenticate the same way. All responses are JSON; errors
are `{code, message, fix?, details?}` with a stable `code`. The main routes:

| Route | Purpose |
|---|---|
| `GET /v1/fleet`, `PUT /v1/fleet` | Read the Fleet and its version; replace it (`If-Match`, `?dryRun=true`) |
| `GET /v1/status` | Agent computers, Services, inboxes, power |
| `GET`, `PUT`, `PATCH`, `DELETE /v1/agent-computers/{c}` | Read, create or replace, change, remove one agent computer |
| `POST /v1/agent-computers/{c}/inbox` | Enqueue (`GET`, `DELETE …/inbox/{id}`, `…/inbox/{id}/events`, `…/inbox/clear`, `…/inbox/hold`, `…/inbox/resume`) |
| `GET /v1/agent-computers/{c}/events` | The agent computer's event stream (SSE, `Last-Event-ID`, `?turn=`) |
| `POST /v1/agent-computers/{c}/turns/{id}/stop` | Stop a Turn |
| `POST /v1/agent-computers/{c}/power` | Sleep or wake |
| `GET /v1/drives`, `DELETE /v1/drives/{d}` | Home, keep and state drives |
| `/v1/tokens` | Scoped tokens |
| `GET /v1/credentials`, `PUT`, `DELETE /v1/credentials/{name}` | List credential names, set or remove a value |
| `/v1/services/…` | Service list, status, create, change, remove, logs, power |

### mTLS listener

Agent computers and Services call the Controller with their certificates. Only the live certificate for a running
container is accepted. Services and agent computers with `grants.send` may enqueue and read or withdraw their own
Messages. Nothing else exists on this listener.

## Docker driver

| Thing | Docker resource |
|---|---|
| Agent computer `C` | Container and internal network `a12s-<fleet>-C`; home drive volume `a12s-<fleet>-home-C`; a volume `a12s-<fleet>-keep-C-…` per kept path of an attached folder |
| Harness bundles | Read-only cache volume `a12s-<fleet>-bundles` |
| Controller | Container `a12s-<fleet>-controller`, its state volume, bridge network `a12s-<fleet>` |
| Inboxes | Volume `a12s-<fleet>-data` |
| Service `S` | Container and network `a12s-<fleet>-S`; state drive `S-state` |

Resources carry `a12s.*` labels. Internal networks are small subnets from a pool (default `10.213.0.0/16`, a /29
each), so many agent computers don't exhaust Docker's default pool. There are no host ports. Containers run with Docker's
`--init` so orphaned harness children are reaped.

## GCE driver (experimental)

Each agent computer is a Compute Engine VM running Container-Optimized OS and the agent computer's container in its
own Docker engine, with the same container settings as above (both drivers build containers with
`internal/driver/engine`). The Controller runs on a VM in the fleet's subnet, with its container on the host network:
agent computer VMs reach its proxy and mTLS listener at its address, and the owner API listens on loopback.

| Thing | GCE resource |
|---|---|
| Agent computer `C` | VM `a12s-<fleet>-C`, tag `a12s-computer`, labels `a12s-fleet`, `a12s-computer`, `a12s-generation`, `a12s-spec`, `a12s-failure` |
| Home drive | pd-balanced disk `a12s-<fleet>-home-C`, attached as device `home` |
| Identity (optional) | service account `<fleet>-C…-xxxx`, no keys, no roles |
| Images and bundles | copied from the Controller VM's engine into each VM's engine |

The Controller drives each VM's engine over mutual TLS (port 2376) and reaches computerd on 7443. A boot script run
by cloud-init on every boot mounts the home disk, installs the in-VM firewall, makes the engine's key and opens the
engine. A change to a stopped agent computer is recorded (the spec on the Controller's state disk, its generation in
the VM's labels) and the container is rebuilt at the next wake. Egress is attributed by the connection's source
address, which VPC anti-spoofing makes the VM's own. See [GCE driver](drivers/gce.md).

## Inside an agent computer

- **Mounts:** the home drive at `/home` (`/home/agent` for the agent; `/home/.a12s` root-only for the journal and
  session record), the bundle cache read-only at `/opt/a12s/bundles`, tmpfs
  `/run/a12s` (secrets, TLS, computerd's socket) and `/tmp`. Each attached folder is a bind mount at
  `/home/agent/<name>` (read-only when asked), with each kept path's volume mounted over its path inside it;
  computerd checks that each attached folder (`A12S_ATTACH`) and kept volume (`A12S_KEEP`) is mounted exactly at its
  path, with no symlink on the way, and refuses to start otherwise; it gives the root of each kept volume to uid 1000
  before the harness starts.
- **Instructions and files** are copied onto the home drive as one tar before the first start and again when they
  change, with a root-owned marker in `/home/.a12s-files` naming the applied digest. A copy overwrites those files
  and leaves the rest of the home drive alone.
- **computerd** (`/opt/a12s/computerd`) waits for its TLS files, listens with mTLS on `:7443`, keeps the journal
  (`/home/.a12s/journal.db`, SQLite) and runs the harness as uid 1000.
- **Environment** set by the driver includes `A12S_COMPUTER`, `A12S_FLEET`, `A12S_HARNESS_KIND`,
  `A12S_HARNESS_VERSION`, `A12S_MODEL`, `A12S_BUNDLE`, `A12S_CREDENTIALS`,
  `A12S_PROXY` and `A12S_CONTROLLER`, plus `A12S_SERVICE_<SVC>_<PORT>` for each used Service port.

### computerd API

| Call | Shape |
|---|---|
| `POST /v1/turns` | `{requestId, session: current\|new\|<id>, prompt, cwd, inbox?}` → `{turnId, sessionId}` once durably accepted. One Turn at a time (`409` when busy). A repeated `requestId` with the same body returns the original Turn. |
| `GET /v1/events` | One SSE stream per agent computer; `Last-Event-ID` and `?turn=`. An expired cursor gets an explicit `gap`. |
| `GET /v1/turns/{id}` | A Turn's summary: dispatched, resume failed, finished, error. |
| `POST /v1/turns/{id}/stop` | Idempotent stop; it also ends the session's background work. |
| `POST /v1/stop` | Stops whatever Turn runs and the session's background work. |
| `GET /v1/status` | Phase, session, active Turn, background task count, harness version, credential generation, owed continuation. |
| `POST /v1/diagnostics`, `POST /v1/journal/inbox` | The Controller journals diagnostics and ended Messages. |
| `POST /v1/continuation/cancel` | Drop an owed `restart_harness` continuation (used by `inbox resume --new`). |

### Adapters

- **Claude Code:** the Claude Agent SDK (`query()`) in streaming-input mode driving the bundle's `claude` binary as
  uid 1000, with an explicit session id and `resume`. User, project and local settings load, so the agent's own
  skills and MCP config work. One process per session, kept across Turns so background sub-agents and shells
  survive them: a Turn is one prompt pushed into it (priority `later`, so it never joins a running turn), ending at
  its `result`. The CLI's `command_lifecycle` frames tell a pushed prompt's turn from one the CLI starts by itself
  when background work ends; computerd journals the latter as a Turn with reason `background` once it calls the
  model. `task_started` / `task_notification` frames of background tasks become `task.started` / `task.finished`;
  the count in status follows the CLI's full live set (`background_tasks_changed`), and a task that leaves it with
  no notification is finished after 2 s.
  Ending the session closes stdin (the CLI stops its tasks and exits); a running turn is interrupted first, then
  SIGTERM after 10 s and SIGKILL after 5 s more.
- **Codex:** `codex app-server` over JSON-RPC on stdio, one long-lived child per agent computer: `thread/start` or
  `thread/resume`, `turn/start`, `turn/interrupt`. A command still running when its turn completes stays a
  background terminal: it is a background task until its late `item/completed`. After a failed or interrupted turn
  nothing is counted, since no late completion is guaranteed. Stop and a new thread restart the
  app-server, which ends them. Every server request is answered: approvals are granted (approval
  policy `never`, `danger-full-access`), user input and elicitation are declined.
- Both get the `a12s` system tools MCP server; each used Service's MCP port is registered as `a12s-<svc>` through a
  stdio-to-HTTP bridge.

A Turn counts as **dispatched** when the adapter has evidence the harness received the prompt (Claude Code's echo of
the user message; Codex's `turn/start` response). The inbox uses this to decide whether a Batch was delivered.

## Inside a Service

A Service has no computerd. Its entrypoint is `a12s service-init` from a read-only `a12s-<fleet>-init` volume, which
prepares the state mount and tmpfs paths, waits for injected secrets, exports `config.env`, starts any TCP
forwarders, then drops to the manifest uid with no capabilities and execs the image's own entrypoint. See
[Services](services.md).
