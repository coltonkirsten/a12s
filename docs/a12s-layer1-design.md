# a12s, Layer 1: Agent Computers

*Design doc, 2026-10-03. Led by Raven, with debate input from Astra (GPT-6 Astra, xhigh) and Fable 5.1. This doc only, nothing is built. The full exchange is in `debate-transcript.md`. a12s (formerly Nexus 2) is the project name, decided 2026-10-05.*

## 1. Goal and non-goals

**Goal.** One file in git declares several agent computers. One command runs them. You prompt any of them and watch the reply stream back. Put them to sleep and they cost nothing. Wake them and they pick up the same conversation. The same file runs on one laptop with Docker or across a company on Kubernetes.

**Layer 1 does:**
- Declare computers: harness, image tier, resources, home preset, drives, credentials, and egress.
- Two harnesses on day one: Claude Code and Codex.
- Prompt in, progress stream out, stop.
- Sleep stops the processes and keeps the disk. On wake, the harness resumes its own session.
- Security from the first commit: credentials injected at start, non-root, enforced egress, authenticated callers.
- A new harness version or model never means rebuilding an image.

**Layer 1 does not do:**
- Queues, teams, messaging between agents, schedules, budgets, approvals, kanban. These come in later layers.
- RAM checkpoints, live migration, warm pools.
- Automatically replaying an interrupted turn. That would be a queue.
- Converting conversations between harnesses.
- An inference broker that keeps the real API key off the box. The seam is designed now and it ships later.

## 2. Core concepts

Every noun has one meaning. None of them collides with a Docker or Kubernetes word it maps onto.

| Name | What it is | Why it exists |
|---|---|---|
| **Fleet** | The spec file (`kind: Fleet`): the set of Computers under one controller. | It names the thing the file declares. "Manifest" means any YAML in Kubernetes. |
| **Computer** | One agent's durable machine identity: its home, its drives, its session, its event history. | Colton's own word. Containers and Pods come and go. The Computer stays. |
| **Harness** | The program that runs the agent loop: Claude Code, Codex, later anything else. | a12s is a meta harness, so the brain is swappable. |
| **HarnessBundle** | A pinned, verified, read-only package of one harness version for one CPU architecture. It ships as an OCI image. | It kills Nexus v1's failure, where every new model meant rebuilding every image. |
| **Adapter** | Code inside the box that makes one Harness speak the a12s contract. | One Harness may have more than one Adapter (native now, ACP later). |
| **Drive** | A persistent volume. Each Computer has exactly one home drive and any number of work drives. | "Attach a drive" reads naturally. "Volume" means something ephemeral in Kubernetes. |
| **Driver** | The substrate backend: `docker`, then `kubernetes`. | Same interface, different substrate. |
| **Controller** | The resident process that reconciles the Fleet, holds power state, authenticates callers, and proxies streams. | Something has to keep converging after the CLI exits. |
| **computerd** | The small daemon inside every Computer, and the only thing the Controller talks to. | It names what it serves. "Bridge" was rejected because it collides with Docker networks. |
| **Session** | The harness's own conversation, under its own id. a12s stores the id and never interprets it. | Resume is the harness's job, not ours. |
| **Turn** | One prompt, one stream of events, one terminal event. | It is the unit of work and of the journal. |

## 3. Example spec

This Fleet meets the exit gate: four Computers, both harnesses, slim and browser.

```yaml
apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: lab }

defaults:
  resources: { cpu: 1, memory: 2Gi }
  home: { size: 10Gi }
  autonomy: full                     # maps to each harness's native permission flags
  egress: { allow: [] }              # deny by default

drives:
  a12s-repo: { size: 20Gi, access: exclusive }
  ux-scratch: { size: 5Gi,  access: exclusive }
  reference:  { size: 5Gi,  access: shared }   # readOnly attachments only; shared-write allows writers

computers:
  scout:
    harness: { kind: claude-code, version: 2.1.280, model: claude-fable-5-1 }
    image: slim
    credentials: [anthropic]
    home: { preset: ./presets/scout }          # copied once, onto an empty home
    drives: [{ name: reference, at: /work/ref, readOnly: true }]
    egress: { allow: [api.anthropic.com] }

  builder:
    harness: { kind: codex, version: 0.154.0, model: gpt-6-astra }
    image: slim
    credentials: [openai, github]
    drives: [{ name: a12s-repo, at: /work/a12s }]
    egress: { allow: [api.openai.com, github.com, registry.npmjs.org] }

  tester:
    harness: { kind: claude-code, version: 2.1.280 }
    image: browser
    resources: { cpu: 2, memory: 4Gi }
    credentials: [anthropic]
    drives: [{ name: ux-scratch, at: /work/ux }]
    egress: { allow: [api.anthropic.com, staging.example.com] }

  reviewer:
    harness: { kind: codex, version: 0.154.0 }
    image: slim
    credentials: [openai]
    drives: [{ name: reference, at: /work/ref, readOnly: true }]
    egress: { allow: [api.openai.com] }
```

**Defaults.** Every Computer inherits `defaults` and can override any field. Maps merge key by key. Lists replace: a Computer's `egress.allow` replaces the default list instead of adding to it.

Things deliberately left out of the file:
- **Power state.** Sleep is an operation, not a definition, and a nap shouldn't need a git commit. A field that only works at creation would make the file lie later. To create a Computer asleep, use `a12s up --asleep`.
- **Where secrets come from.** The Fleet only names credentials. A per-deployment bindings file, which is not portable, maps each name to its source: an env var or file on a laptop, a Kubernetes Secret or External Secrets on a cluster. A laptop path in git means nothing to a cluster.
- **Exact digests.** `fleet.lock` pins image, bundle, and preset digests. Moving a version is an explicit lock update.

Scaling is editing `computers:` and running `a12s up` again.

## 4. The driver interface

The model is desired state. One reconciler in the Controller has all the opinions, and the Drivers are simple and idempotent.

| Method | Semantics |
|---|---|
| `capabilities()` | Report what the substrate can enforce: shared drives, egress enforcement, architectures, sandboxed runtime. Gaps become admission errors, not surprises at runtime. |
| `observe(scope)` | The resources a12s owns, with their generation, power, readiness, and failures. |
| `watch()` | Push events: died, OOM-killed, evicted. Docker events and Kubernetes informers. |
| `ensureDrive(id, spec)` | Create or verify a drive. Never shrinks it, never erases it. |
| `stageComputer(id, generation, spec)` | Prepare the definition: bundle, mounts, network, policy. Never touches a running Computer. |
| `setPower(id, awake\|asleep, generation)` | Start the staged generation, or stop all of the Computer's processes. |
| `connect(id, service)` | An authenticated channel to `computerd`. |
| `exec(id, cmd)` | Privileged, audited debug shell. Never on the harness path. |
| `removeComputer(id)` / `removeDrive(id)` | Removing a Computer keeps its drives. A drive is destroyed only by an explicit command, and only when nothing has it attached. |

Every mutation carries a **generation**. Stale writes are rejected, and success is read back through `observe`, never assumed because a request was accepted.

**Docker:** one container per Computer and one isolated internal network per Computer. Subnets are small, allocated from a configured pool, because Docker's default pool runs out at about 30 networks. There are no host ports. Drives are named volumes. `a12s up` starts the Controller container, applies the Fleet, and exits.

**Kubernetes:** one StatefulSet per Computer at replicas 0 or 1. Zero replicas is the sleep primitive, and the PVCs and stable name survive it. Each Computer gets a headless Service. The Controller runs as a Deployment running the same reconciler. kubernetes-sigs/agent-sandbox could become a third Driver later. It is not a dependency.

**Drive access.** A drive is `exclusive` (one Computer reads and writes), `shared` (any number attach, all read-only), or `shared-write` (any number attach and write, like a team drive). `shared-write` needs storage that every node can write at once. That's free on Docker, and on Kubernetes it means a ReadWriteMany storage class, reported by `capabilities()`. Where it's missing, the reconciler refuses the Fleet instead of attaching. Writers coordinate through the `lock` and `release` system tools (section 5). Locks prevent two writes to the same file at once, not one agent overwriting another's work later. For code, use git.

**Config changes on a busy Computer.** Staging happens right away. The change is applied at the next idle boundary, with no automatic deadline, so a clock never kills a six-hour migration. For an urgent fix, such as a CVE in an image, an operator runs `a12s apply --interrupt --grace 30s`. The deadline belongs to a human action, not to the reconciler.

## 5. The harness contract

The contract is tiny on purpose: prompt in, progress out, stop. It is HTTP plus SSE between the Controller and `computerd`, over mTLS with a certificate per instance.

| Call | Shape |
|---|---|
| Prompt | `POST /v1/turns {requestId, session: current\|new\|<id>, prompt, cwd}` returns `{turnId, sessionId}` once the turn is durably accepted. |
| Stream | `GET /v1/events` is one SSE stream per Computer. It takes `Last-Event-ID` and an optional `?turn=` filter. |
| Stop | `POST /v1/turns/{id}/stop` is idempotent. |
| Status | `GET /v1/status` returns phase, session, active turn, harness version, and credential generation. |

**Rules:**
- **One turn at a time.** A busy Computer returns `409` and a sleeping one returns `503`. Nothing is queued, because queues belong to Layer 2.
- **Idempotent prompts.** Repeating a `requestId` with the same body returns the original turn. Changing the body is a conflict. Without this, a lost HTTP response plus a client retry runs "publish the release" twice.
- **One stream per Computer, not per Turn.** Some events have no turn, such as `resume.failed` or a lifecycle change. A stream per turn would also race the subscription against the first events of every turn.
- **A journal.** Every event goes to an append-only SQLite journal on the home drive, so the sequence survives container replacement. Reconnecting replays the journal. An expired cursor returns an explicit gap. This is Nexus v1's branch journal, kept. `computerd` runs as its own user and is the journal's only reader and writer. The journal sits in a home-drive directory the agent's user can't open, so an agent or a prompt injection can't read, edit, or erase its own record.

**Event envelope.** `{v:1, computer, session, turn, seq, ts, type, data}`. The types:
- `turn.started` (harness version, model, `resumed: true|false`)
- `text.delta`
- `tool.started` / `tool.finished`
- `permission` (the policy's decision, for audit)
- `diagnostic` (including denied egress, so a human sees exactly what was blocked)
- `turn.finished` (`completed | failed | interrupted`, usage)
- `session.changed`, `resume.failed`, `lifecycle`

**Adapters.** Native adapters come first. Resume is the whole point of Layer 1, and the native CLIs own their session files.
- **Claude Code:** `claude -p` with stream-json in and out, an explicit session id, and `--resume`. Stop sends SIGTERM, then kills after a deadline.
- **Codex:** `codex app-server` over JSON-RPC on stdio: `thread/start` or `thread/resume`, `turn/start`, `turn/interrupt`.
- **ACP** is the third adapter and the plug-in point for custom harnesses. It is not the default.

**Permission prompts.** Layer 1 has nobody to answer them, so `autonomy` lives in the spec and maps onto native flags. `full` covers Claude's permission mode and allowed tools and Codex's approval policy and sandbox. `restricted` denies automatically. Every request gets a decision and emits a `permission` event, so a turn never hangs on a question nobody can answer. The URL for an answer verb is reserved and nothing ships behind it yet.

**Getting the harness in.** It is never baked into an image and never installed onto the home drive. The HarnessBundle is an OCI image, auto-built by a12s CI when an upstream release lands, run through a conformance test, and pinned by digest in `fleet.lock`.
- Each bundle declares the hosts its harness needs to sign in, for subscription login (Claude, ChatGPT) or an API key. The Controller adds them to that Computer's egress allowlist automatically, so a spec only lists the hosts its work needs.
- **Kubernetes** uses the bundle image as an init-container, so the kubelet caches it per node.
- **Docker** extracts it into a read-only cache volume.

Bumping Claude Code means editing one line and restarting. No image rebuild. A writable install would let a prompt injection patch its own harness and keep the patch through every sleep. It would also need npm in every egress list.

**System tools.** `computerd` gives the agent a small MCP server for managing its own environment. Skills, MCP configs and harness settings live on the home drive, so the agent can write them. The read-only bundle is only the harness program. Today there are three tools:

- `restart_harness`: restarts the harness when the current turn ends, resumes the same session, and starts a continuation turn so the agent carries on. That lets the harness pick up skills and MCP servers the agent just wrote, for example to test an MCP it built. It emits a `harness.restarted` event.
- `lock` and `release`: an advisory lock on a path in a `shared-write` drive. The Controller holds it as a lease, so it works across nodes and expires if the holder dies. It's advisory: plain file writes ignore it.

## 6. Security model

- **No secrets in images, config, or disk.**
  - On Docker, the Controller writes credentials through Engine exec over stdin into a tmpfs inside the container (`/run/a12s/secrets`, mode 0400, owned by the agent user). Nothing goes into `docker inspect`, argv, or the home drive.
  - On Kubernetes they are native Secrets, mounted read-only, with RBAC and encryption at rest.
  - `computerd` passes them only to the harness process.
  - Claude OAuth refresh files are symlinked onto tmpfs.
- **Rotation is confirmed, not assumed.** `computerd` reports the credential generation it has actually observed, and the old key is revoked only after that. The kubelet syncs Secrets with a delay. Rotation also restarts the harness process, because a long-lived Codex server would otherwise keep the revoked key. For urgent revocation, the active turn is interrupted.
- **Instance identity.** The Controller is a built-in CA, with no external PKI on a laptop. Each Computer generation gets its own certificate, delivered through the same bootstrap path as credentials. This encrypts prompts and repo contents on the wire between nodes. It also binds egress policy to identity, not to an IP address that can be reassigned.
- **Egress is enforced, not hinted.**
  - Each Computer's only route out is a validating egress proxy. On Docker that means an isolated internal network with isolated gateway mode, so there is no path to host services such as an exposed Docker API. On Kubernetes it means default-deny NetworkPolicy plus the proxy.
  - The proxy allows only the declared hosts and HTTPS ports. It rejects private and link-local addresses and DNS rebinding. Raw UDP, external DNS, and SSH-git are blocked; agents use HTTPS git.
  - `allow: ["*"]` permits any public host on any port, for experiments. Private, link-local and cloud metadata addresses stay blocked, so a permissive Computer still can't reach the host, other Computers, or instance credentials. It is printed on every status line.
  - If the substrate can't enforce egress (kind, Docker Desktop's Kubernetes), a Fleet-level `egress.enforcement: unenforced` is accepted and printed on every status line. YAML never pretends to be a firewall.
- **Least privilege.** The agent runs as uid 1000 with a read-only root filesystem. It has no privilege escalation, no added capabilities, no Docker socket, and no host mounts. Only the home drive, work drives, and tmpfs are writable.
- **Callers are authenticated in Layer 1.** Locally that is an owner-only Unix socket. On a cluster, every caller presents a token that carries an allowlist of Computers and actions (prompt, shell, purge). Roles and SSO come later. Without this, any pod in the namespace could drive the browser Computer.
- **Honest limits.** Containers share a kernel. Protecting tenants that are hostile to each other needs a sandboxed runtime (gVisor or Kata), which the Driver reports through `capabilities()`. Any host on the allowlist is still a possible exfiltration channel.

**Image tiers.** There are exactly two, plus custom image refs.
- **slim:** Debian slim, certificates, git, ripgrep, python3, Node, and computerd.
- **browser:** slim plus headless Chromium, fonts, the Playwright library, and the Playwright MCP server preconfigured, plus a shipped seccomp profile so Chromium keeps its own sandbox without `CAP_SYS_ADMIN`.

The agent drives the browser through Playwright MCP (navigate, click, type, read the page as an accessibility snapshot, screenshot), which works the same for Claude Code and Codex. It can also write Playwright scripts for repeatable checks. The harnesses' own browser tools are not used, because they expect a real desktop and differ per harness. Browser traffic goes through the egress proxy like everything else. A full desktop tier (Xfce, KasmVNC, a human view through the Controller gateway) comes later. Toolchain-heavy images are derived images, not a third tier.

## 7. Lifecycle

Controller states: `Provisioning → Starting → Ready ⇄ Busy → Stopping → Asleep`, plus `Error`, which carries a reason and whether a retry can help.

**Sleep** (`a12s sleep tester`):
1. Close admission.
2. Interrupt the active turn, which ends `interrupted`.
3. Allow a bounded flush of about 20 seconds.
4. Stop: `docker stop` on Docker, replicas 0 on Kubernetes.

After sleep, the Computer uses no CPU or RAM. Its storage, and the shared Controller and proxy, still cost a little.

| Survives sleep | Does not survive |
|---|---|
| Home drive: session files (`~/.claude/projects`, `~/.codex/sessions`), journal, user-installed tools, browser profile. Work drives. The session id. | RAM, background processes, `/tmp`, injected credentials, certificates. |

**Wake:**
1. Start the container.
2. Re-inject credentials and certificate.
3. `computerd` checks the bundle and reports `Ready (resumePending)`.

Resume is **lazy**: the next prompt reopens the native session (`--resume <id>`, `thread/resume`). Where the session can be checked without running a model turn, a best-effort precheck runs at wake. That covers Codex `thread/resume` and a Claude session-file format check. If the precheck fails, `resume.failed` is emitted early, while an operator is still paying attention.

**If resume fails,** a12s never silently starts fresh. The old session id and disks are kept, and `resume.failed` is emitted. Only two ways forward are accepted:
- an explicit `session: new`
- a repair, such as rolling back the bundle, followed by a retry

The reason: "continue the approved migration," sent into a fresh session, would run without the approval context.

**Interrupted turns stay interrupted.** Side effects may already have happened, so there is no automatic replay. The conversation survived, so "continue" works.

**Removing a Computer from the Fleet** stops it and keeps its drives forever. `a12s status` lists orphaned drives loudly. Only `a12s drive rm <name>` destroys data. Checking out a branch with fewer Computers must never start a countdown that ends in deletion.

## 8. The exit gate

Layer 1 is done when all of this runs on mac-mini's Docker and a log shows it:

1. **One file.** The Fleet above declares four Computers: Claude Code and Codex, slim and browser.
2. **One command.** `a12s up` brings all four to `Ready`.
3. **Streamed reply.** `a12s prompt <any> "…"` streams `text.delta` events to a `turn.finished`, for each of the four.
4. **Sessions resume.** Tell scout a word, run `a12s sleep --all` then `a12s wake --all`, and ask for the word back. All four answer from the same session (`resumed: true`).
5. **Scaling is editing.** Add a fifth Computer to the file, run `a12s up`, and only that one is created. Remove it and its drives are kept.

Astra and Fable both argued to add these, and I agree:

6. Kill the SSE client mid-turn and reconnect with `Last-Event-ID`. No events are lost or duplicated.
7. A request to a host that isn't allowed is blocked, and the block shows up as a `diagnostic` event.
8. Bump the harness version in the lock. The next turn uses it, and no image is rebuilt.

## 9. Where Astra and Fable disagreed, and what we chose

Both models converged on most points across two rounds. These were the real fights.

- **Resume, eager or lazy.** They swapped sides mid-debate. **Chosen: lazy resume, plus a precheck at wake where one exists.** Claude Code can't confirm a session without a prompt, so a fully eager check would be fake for half the fleet. Both agreed that silently starting fresh is never allowed.
- **Getting the harness in.** Fable first proposed installing it from npm onto the home drive at boot. **Chosen: Astra's read-only bundle, packaged as an OCI image, with Fable's refinement that CI builds it automatically.** Fable conceded on security: a writable harness lets a prompt injection persist, and it forces npm into every egress list.
- **Transport auth.** Fable wanted a bearer token over a private network; Astra wanted mTLS. **Chosen: mTLS, with the Controller as a zero-config CA.** On a multi-node cluster, tool output (repo contents) would otherwise cross nodes in plaintext. Once the Controller exists, it costs nothing extra on a laptop.
- **Power in the file.** Astra wanted `initialPower`; Fable wanted no power field at all. **Chosen: no power field, and `a12s up --asleep` instead.** The file must always mean desired state.
- **Credentials.** Fable put secret sources in the spec as a list; Astra used one provider in a separate binding. **Chosen: a list of names in the Fleet** (a browser agent needs a model key plus a GitHub token), **with sources in per-deployment bindings**, so the file stays portable.
- **Config changes mid-turn.** **Chosen: wait for idle with no automatic deadline. Interrupting is a human verb.**
- **Egress off-switch.** Astra wanted it rejected outright; Fable said that walls off every local Kubernetes. **Chosen: allowed, and loud on every status line.**
- **Names.** Box/Agent vs Computer, Fleet vs Manifest, Drive vs Volume, Bridge vs boxd vs steward vs computerd. **Chosen: Computer, Fleet, Drive, computerd.** Each one avoids clashing with a Docker or Kubernetes noun or with another a12s noun.

## 10. Open questions for Colton

1. **Credentials for the exit gate.** Both models lean toward API keys first. Running several agents on one Claude subscription OAuth token is a terms-of-service question before it is a technical one. Is that OK, or should we use API keys with hard limits? **Decided 2026-10-05: subscriptions first (Claude and ChatGPT sign-in), for Colton's own fleet only. API keys later use the same bundles with a different credential.**
2. **Shell access.** Do you want `a12s shell <name>`, audited and outside the harness path? Both lean yes. **Decided 2026-10-05: yes.**
3. **Autonomy default.** Is `full` (the agent owns its computer and approves its own tools) the right default, with `restricted` opt-in? **Decided 2026-10-05: yes, `full` is the default.**
4. **Desktop base.** Should we build our own non-root desktop image (more upkeep, stricter security), or accept Webtop and relax the rules for that tier only? My lean is to build our own. **Decided 2026-10-05: no desktop in Layer 1. A `browser` tier (headless Chromium plus Playwright MCP) ships instead; the desktop tier comes later.**
5. **Repo and license.** A new public repo from day one? And which license: Apache-2.0 (my lean, for enterprise friendliness), or AGPL? **Decided 2026-10-05: a private repo for now, MIT license.**
6. **Name.** Three candidates:
   - **Roost:** where ravens sleep and wake. The core verb of Layer 1 is the brand.
   - **Hangar:** where machines are parked at zero cost and rolled out ready.
   - **Bivouac:** a light, self-contained camp for each agent. Distinctive and easy to search.

   **Decided 2026-10-05: a12s** (a numeronym for "agent computers", like k8s). Colton bought a12s.ai. a12s.dev and a12s.sh were open; the GitHub handle a12s is taken by an inactive account.

## Appendix A. The command line

Layer 1's human interface is the `a12s` CLI. It talks to the Controller's authenticated API, the same API a dashboard or a Layer 2 orchestrator will use later. No UI ships in Layer 1.

```text
a12s up [--asleep]                  apply the Fleet file: create, update or stage Computers
a12s apply --interrupt --grace 30s  force a staged change onto a busy Computer
a12s status [<name>]                state, session, active turn, harness version, warnings
                                     (orphaned drives, unenforced egress, allow "*")

a12s prompt <name> "…"              send a turn and stream its events
a12s prompt <name> --new "…"        start a fresh session instead of continuing
a12s stop <name>                    interrupt the active turn
a12s logs <name> [--follow] [--since <seq>]   replay and follow the journal

a12s sleep <name> | --all           sleep Computers (storage kept, no CPU or RAM)
a12s wake <name> | --all            wake Computers; resume happens on the next prompt

a12s shell <name>                   audited terminal inside a Computer, outside the harness

a12s drive ls                       drives, sizes, what's attached, orphans
a12s drive rm <name>                the only command that destroys data
```

Example session:

```text
$ a12s up
created scout, builder, tester, reviewer (asleep: none)
$ a12s prompt builder "add a health check endpoint and open a PR"
turn t_41 started (codex 0.154.0, gpt-6-astra, resumed: false)
… streamed text and tool events …
turn t_41 completed
$ a12s sleep --all
$ a12s status
scout     Asleep   session s_9c…   home 10Gi
builder   Asleep   session s_2f…   home 10Gi  drive a12s-repo
```

