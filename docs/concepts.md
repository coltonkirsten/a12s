# Concepts

Every noun in a12s has one meaning, chosen so it doesn't collide with a Docker or Kubernetes word.

| Name | What it is |
|---|---|
| **Fleet** | The spec file (`kind: Fleet`, usually `fleet.yaml`) and the set of Computers and Services it declares under one Controller. |
| **Computer** | One agent's durable machine: its home drive, work drives, harness session, event journal and inbox. Containers come and go; the Computer stays. |
| **Harness** | The program that runs the agent loop: Claude Code (`claude-code`) or Codex (`codex`). |
| **HarnessBundle** | A pinned, read-only package of one harness version for one CPU architecture, shipped as an OCI image. |
| **Adapter** | Code inside computerd that makes one harness speak the a12s contract. |
| **Drive** | A persistent volume. Each Computer has one home drive and any number of work drives. |
| **Controller** | The resident process that reconciles the Fleet, holds power state, owns the inboxes, authenticates callers and proxies event streams. |
| **computerd** | The small daemon inside every Computer and the only thing the Controller talks to there. |
| **Session** | The harness's own conversation, under its own id. a12s stores the id and resumes it; it never interprets it. |
| **Turn** | One prompt in, one stream of events out, ending in one terminal event. |
| **Inbox** | The Controller-owned, durable queue of Messages for one Computer. |
| **Message** | One enqueue: a body, a sender label, the verified principal (`via`) and optional metadata, expiry and replace key. |
| **Batch** | The pending Messages delivered together as one Turn. |
| **Service** | A non-agent container run from the same Fleet: no harness, session or inbox. |
| **Grant** | A capability written in the Fleet on the side that acts: `grants.send` (may enqueue to these Computers), `uses` (may reach these Services). |
| **Principal** | The authenticated identity of an API caller: `local`, `token:<name>`, `service:<name>` or `computer:<name>`. |

## How the pieces fit

```text
 you ── a12s CLI ──(loopback, bearer token)──▶ Controller ──(mTLS)──▶ computerd ──▶ harness (Claude Code / Codex)
                                                  │                       │
                                       fleet.db (inboxes)           journal.db on the home drive
                                       egress proxy  ◀── all outbound traffic from every Computer and Service
```

`a12s up` starts the Controller container and hands it the Fleet. The Controller creates one container and one
isolated internal network per Computer and Service. Each Computer's only route out is the Controller's egress
proxy. See [Architecture](architecture.md) for the details.

## Lifecycle

A Computer moves through `Provisioning → Starting → Ready ⇄ Busy → Stopping → Asleep`, plus `Error`, which carries a
reason.

**Sleep** closes admission, interrupts the active Turn (it ends `interrupted`), allows a short flush and stops the
container. An asleep Computer uses no CPU or RAM.

| Survives sleep | Does not survive |
|---|---|
| The home drive: session files (`~/.claude/projects`, `~/.codex/sessions`), the journal, tools the agent installed, the browser profile. Work drives. The session id. The inbox. | RAM, background processes, `/tmp`, injected credentials, certificates. |

**Wake** starts the container, re-injects credentials and a fresh certificate, and reports `Ready (resumePending)`.
Resume is lazy: the next Turn reopens the native session (`--resume <id>` for Claude Code, `thread/resume` for
Codex). Where a session can be checked without running a model turn, a precheck runs at wake and emits
`resume.failed` early.

**If resume fails,** a12s never silently starts a fresh session: an instruction like "continue the approved
migration" must not run without its context. The old session id and drives are kept and `resume.failed` is
emitted. Recover with an explicit new session (`a12s prompt <name> --new`, or `a12s inbox resume <name> --new`) or
repair the cause (for example roll back the bundle) and retry.

**Interrupted Turns stay interrupted.** Side effects may already have happened, so nothing is replayed
automatically. The conversation survives, so "continue" works.

## Data safety

Removing a Computer from the Fleet stops it and keeps its drives and inbox; `a12s status` lists them as orphans.
Plain `a12s down` removes the Fleet's containers, networks and Controller state and keeps every drive and the inbox
database; the next `a12s up` reattaches them. Only two commands destroy data, and both ask first:

- `a12s drive rm <name>`
- `a12s down --purge` (lists the drives and the inbox database, `--yes` skips the question)

## Config changes

`a12s up` stages a change right away and applies it to each Computer at its next idle boundary, with no automatic
deadline, so a running six-hour job is never killed by a config edit. For an urgent change (a CVE in an image),
`a12s apply --interrupt --grace 30s` ends the active Turns. Services have no idle boundary, so a changed Service is
restarted at once.

## Event journal

Every event goes to an append-only SQLite journal on the Computer's home drive, in a directory the agent's user
cannot open, so an agent can't read, edit or erase its own record. `a12s logs <name>` replays it; reconnecting with
`Last-Event-ID` resumes without losing or duplicating events.

Every event has the envelope `{v:1, computer, session, turn, seq, ts, type, data}`. Types:

| Type | Meaning |
|---|---|
| `turn.started` | Harness version, model, `resumed`, and the inbox Batch it carries |
| `turn.dispatched` | The harness has received the prompt |
| `text.delta` | Streamed text |
| `tool.started` / `tool.finished` | A tool call |
| `permission` | A permission decision, for audit |
| `diagnostic` | Something an operator should see, such as a denied egress request |
| `turn.finished` | `completed`, `failed` or `interrupted`, with usage |
| `session.changed`, `resume.failed`, `lifecycle`, `harness.restarted` | Session and lifecycle changes |
| `inbox.discarded` | A Message that ended without delivery (cleared, withdrawn, expired, replaced), body included |

## System tools

computerd gives the agent a small MCP server, `a12s`, for managing its own environment:

- `restart_harness`: restart the harness when the current Turn ends, resume the same session and continue. Use it
  after writing new skills or MCP config to the home drive.
- `lock` / `release`: an advisory lock on a path in a `shared-write` drive, held as a Controller lease that
  expires if the holder dies. Plain file writes ignore it.
- `send`: enqueue a Message to another Computer, present only when the Computer has `grants.send`.

Skills, MCP configs and harness settings live on the home drive, so the agent can change them; the harness program
itself is read-only.

## Permissions

There's nobody to answer a permission prompt, so `autonomy` in the Fleet maps onto each harness's native settings:

- `full` (default): the agent owns its computer. Claude Code runs with `bypassPermissions`; Codex with approval
  policy `never` and `danger-full-access`.
- `restricted`: every permission request is denied automatically (Claude Code `dontAsk`; Codex `on-request` with
  `workspace-write` and auto-decline).

Every decision emits a `permission` event, so a Turn never hangs on a question.
