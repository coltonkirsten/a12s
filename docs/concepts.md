# Concepts

Five ideas are enough to build with a12s:

| | What it is |
|---|---|
| **Fleet** | The agent computers (and Services) one project runs. `fleet.yaml` declares it and `a12s up` makes the running Fleet match the file; the SDK adds, changes and removes agent computers while it runs. |
| **Agent computer** | One agent's durable machine (`AgentComputer` in code): a harness (Claude Code or Codex), a home drive with its instructions, a conversation (session) and an inbox. Containers come and go; the agent computer stays. |
| **Message** | Work you put in an agent computer's inbox (`send`). Anyone allowed can send: you, your app, a Service, or another agent computer with `grants.send`. |
| **Turn** | One delivery of work: the agent computer takes everything pending in its inbox, runs the agent once, and ends `completed`, `failed` or `interrupted`. |
| **Event** | What an agent computer journals while it works: `turn.started`, streamed `text.delta`, `tool.started`, `turn.finished`, … Your app follows them live. |

The everyday loop: **edit `fleet.yaml` or an agent computer's instructions → `a12s up` → send a Message → follow its
Turn.** From code: **create an agent computer → `send(...).wait()` → remove it.**

```text
            send                   delivered as a Turn            events
 your app ─────────▶ inbox of lead ─────────────────▶ lead (agent) ─────────▶ your app
                                                         │ send (system tool, needs grants.send)
                                                         ▼
                                                inbox of worker1 ─▶ worker1 ─▶ … ─▶ inbox of lead
```

Agents talk to each other the same way you talk to them: an agent computer whose Fleet entry has
`grants.send: [worker1]` gets a `send` tool (`mcp__a12s__send`, arguments `computer` and `body`). The Message lands
in worker1's inbox, and worker1's next Turn delivers it, labeled `via="computer:lead"`. Replies go back the same way.
Each agent computer keeps its conversation between Turns, so a lead remembers what it handed out. [Recipes](recipes.md#lead-and-workers)
puts this together.

## Everything else, briefly

| Name | What it is |
|---|---|
| **Session** | The harness's own conversation, under its own id. It survives sleep; a12s resumes it on the next Turn. `fresh` (or `send --fresh`) starts a new one. |
| **Batch** | The pending Messages one Turn delivers together. |
| **Principal** (`via`) | Who really sent a Message: `local` (you, with the owner token), `token:<name>`, `service:<name>` or `computer:<name>`. Agents should trust `via`, not the sender's own label. |
| **Grant** | A permission written on the side that acts: `grants.send` (may send to these agent computers), `uses` (may reach these Services). |
| **Service** | A non-agent container from the same Fleet: a clock, a database, an MCP server, a webhook bridge. See [Services](services.md). |
| **Drive** | A persistent volume: each agent computer's home drive, plus shared work drives. |
| **Instructions**, **files** | Text the Fleet writes onto an agent computer's home drive: `instructions` becomes its `CLAUDE.md` (or `AGENTS.md` for Codex), `files` holds skills, settings and anything else. A **preset** (`home.preset`) is a directory of such files that `a12s up` sends. |
| **Harness**, **HarnessBundle** | The agent program (Claude Code or Codex), shipped as a pinned read-only bundle per version. |
| **Controller** | The process `a12s up` starts in a container: it holds the Fleet and its version, reconciles it, owns the inboxes and serves the API. |
| **computerd** | The small daemon inside every agent computer that runs the harness and keeps the journal. |

## How the pieces fit

```text
 you ── a12s CLI ──(loopback, bearer token)──▶ Controller ──(mTLS)──▶ computerd ──▶ harness (Claude Code / Codex)
                                                  │                       │
                                       fleet.db (inboxes)           journal.db on the home drive
                                       egress proxy  ◀── all outbound traffic from every agent computer and Service
```

`a12s up` starts the Controller container and hands it the Fleet; the SDK and the HTTP API change it from then on.
The Controller creates one container and one isolated internal network per agent computer and Service. Each agent
computer's only route out is the Controller's egress proxy. See [Architecture](architecture.md) for the details.

## Lifecycle

An agent computer moves through `Provisioning → Starting → Ready ⇄ Busy → Stopping → Asleep`, plus `Error`, which carries a
reason.

**Sleep** closes admission, interrupts the active Turn (it ends `interrupted`), allows a short flush and stops the
container. An asleep agent computer uses no CPU or RAM.

| Survives sleep | Does not survive |
|---|---|
| The home drive: session files (`~/.claude/projects`, `~/.codex/sessions`), the journal, tools the agent installed, the browser profile. Work drives. The session id. The inbox. | RAM, background processes, `/tmp`, injected credentials, certificates. |

**Wake** starts the container, re-injects credentials and a fresh certificate, and reports `Ready (resumePending)`.
Resume is lazy: the next Turn reopens the native session (`--resume <id>` for Claude Code, `thread/resume` for
Codex). Where a session can be checked without running a model turn, a precheck runs at wake and emits
`resume.failed` early.

**If resume fails,** a12s never silently starts a fresh session: an instruction like "continue the approved
migration" must not run without its context. The old session id and drives are kept and `resume.failed` is
emitted. Recover with an explicit new session (`a12s send <name> --fresh "…"`, or `a12s inbox resume <name> --new`) or
repair the cause (for example roll back the bundle) and retry.

**Interrupted Turns stay interrupted.** Side effects may already have happened, so nothing is replayed
automatically. The conversation survives, so "continue" works.

## Data safety

Removing an agent computer from the Fleet (from the file, or with `remove()`) stops it and keeps its drives and
inbox; `a12s status` lists them as orphans.
Plain `a12s down` removes the Fleet's containers, networks and Controller state and keeps every drive and the inbox
database; the next `a12s up` reattaches them. Only two commands destroy data, and both ask first:

- `a12s drive rm <name>`
- `a12s down --purge` (lists the drives and the inbox database, `--yes` skips the question)

## Config changes

A change to an agent computer, from `a12s up` or from the SDK's `update()`, is staged right away and applied at its
next idle boundary, with no automatic deadline, so a running six-hour job is never killed by a config edit. For an urgent change (a CVE in an image),
`a12s apply --interrupt --grace 30s` ends the active Turns. Services have no idle boundary, so a changed Service is
restarted at once.

Every change to the Fleet adds one to its version. A whole-Fleet write can name the version it read (`If-Match`)
and is refused if someone wrote in between; a write to one agent computer changes only that one, and the last write
wins.

## Event journal

Every event goes to an append-only SQLite journal on the agent computer's home drive, in a directory the agent's user
cannot open, so an agent can't read, edit or erase its own record. `a12s logs <name>` replays it; reconnecting with
`Last-Event-ID` resumes without losing or duplicating events.

Every event has the envelope `{v:1, agentComputer, session, turn, seq, ts, type, data}` (`computer` is
`agentComputer` under its old name, for one release). Types:

| Type | `data` |
|---|---|
| `turn.started` | `{harness, model, resumed, prompt, cwd, inbox: {batch, messages: [{id, sender, via, …}]}}`: the Messages this Turn delivers |
| `turn.dispatched` | `{}`: the harness has received the prompt |
| `text.delta` | `{text}`: streamed text from the agent |
| `tool.started` | `{id, name, input}`. MCP tools are named `mcp__<server>__<tool>`, e.g. `mcp__a12s__send` |
| `tool.finished` | `{id, name, isError, output}` |
| `turn.finished` | `{status: "completed" \| "failed" \| "interrupted", usage, tokens?, error?}`: `usage` is the harness's own; `tokens` is `{input, output, cacheRead, cacheWrite}` for every harness (`input` excludes cached input) |
| `egress.denied` | `{host, port, reason}`: the egress proxy refused a connection. Every Claude Code Turn has one or two, for telemetry hosts; they are harmless |
| `diagnostic` | `{message, …}`: something an operator should see |
| `permission` | `{decision, kind, tool}`: a permission decision, for audit |
| `session.changed`, `resume.failed`, `lifecycle`, `harness.restarted` | Session and lifecycle changes |
| `inbox.discarded` | A Message that ended without delivery (cleared, withdrawn, expired, replaced), body included |

The fleet-wide stream (`GET /v1/events`) adds two events the Controller makes: `fleet.changed`
`{version, by, names}` when a write changes the Fleet, and `agent_computer.phase` `{phase, error?}` when an agent
computer's phase changes.

The SDKs type these ([SDK](sdk.md)); `turns()` folds them into one record per Turn. The Controller keeps the last 200
finished Turns of each agent computer in the same shape (`GET /v1/agent-computers/{c}/turns`), so a Turn that ended
while nobody watched can still be read.

## System tools

computerd gives the agent a small MCP server, `a12s`, for managing its own environment:

- `restart_harness`: restart the harness when the current Turn ends, resume the same session and continue. Use it
  after writing new skills or MCP config to the home drive.
- `lock` / `release`: an advisory lock on a path in a `shared-write` drive, held as a Controller lease that
  expires if the holder dies. Plain file writes ignore it.
- `send` (`computer`, `body`, and optionally `fresh`, `expires`, `replaceKey`): put a Message in another agent
  computer's inbox. Present only when the agent computer has `grants.send`, and only for those agent computers. It returns a receipt, not
  an answer: the reply, if any, comes back as a new Message in a later Turn.

Skills, MCP configs and harness settings live on the home drive, so the agent can change them; the harness program
itself is read-only.

## Permissions

There's nobody to answer a permission prompt, so `autonomy` in the Fleet maps onto each harness's native settings:

- `full` (default): the agent owns its computer. Claude Code runs with `bypassPermissions`; Codex with approval
  policy `never` and `danger-full-access`.
- `restricted`: every permission request is denied automatically (Claude Code `dontAsk`; Codex `on-request` with
  `workspace-write` and auto-decline).

Every decision emits a `permission` event, so a Turn never hangs on a question.
