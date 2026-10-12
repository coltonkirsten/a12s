# CLI reference

```text
a12s [--fleet|-F fleet.yaml] [--bindings path] [--json] <command> [args]
```

| Global flag | Meaning |
|---|---|
| `--fleet`, `-F` | The Fleet file. Default: `$A12S_FLEET`, then the nearest `fleet.yaml` upward. |
| `--bindings` | The bindings file. Default: `a12s.bindings.yaml` next to the Fleet file. |
| `--json` | Machine-readable output. Listings print JSON; `logs` and `prompt` print events as JSON lines. The JSON is the stable contract; human text may change. |

Exit codes: `0` success, `1` failure, `130` interrupted. Errors end with a `fix:` that says what to do.

## Fleet

| Command | What it does |
|---|---|
| `a12s init [dir] [--harness claude-code\|codex] [--with db,cron]` | Scaffold `fleet.yaml`, `fleet.lock` (pinning this a12s release), an empty bindings file, `.gitignore` and a README. `--with` adds reference Services: `db` (Postgres plus an MCP server), `cron` (a clock). |
| `a12s up [--asleep] [--controller-image ref]` | Start the Controller if needed, validate and apply the Fleet: create, update or stage Computers and Services, pin new digests in `fleet.lock`. `--asleep` creates new Computers asleep. |
| `a12s apply --interrupt [--grace 30s]` | Apply staged changes to busy Computers now, interrupting their Turns after the grace period. |
| `a12s status [<name>]` | Phase, session, active Turn, harness version, inbox, power policy and warnings (orphaned drives, unenforced egress, `allow: ["*"]`, published ports). |
| `a12s down [--purge [--yes]]` | Remove the Fleet's containers, networks and Controller; keep drives and the inbox database. `--purge` also destroys them after listing them and asking. |
| `a12s doctor` | Check the Fleet file, Docker, images and bundles, bindings, the Controller, egress and every Computer's and Service's health. One line per check with the fix; exits 1 on any failure. |
| `a12s lock update services/<svc>` | Re-pin a Service whose image ref hasn't changed. |
| `a12s version` | The a12s release and the registry its images come from. |

## Working with Computers

| Command | What it does |
|---|---|
| `a12s prompt <name> [--new] "…"` | Enqueue a Message and stream the Turn that delivers it. `--new` starts a fresh session. |
| `a12s send <name> "…" [flags]` | Enqueue a Message and exit; works while the Computer is busy or asleep. `-` reads the body from stdin. Flags: `--from`, `--fresh`, `--meta k=v`, `--key`, `--expires`, `--replace` (see [Inbox](inbox.md#message-options)). |
| `a12s stop <name> [--hold]` | Interrupt the active Turn. `--hold` holds the inbox first, so nothing else is delivered. |
| `a12s logs <name> [--follow] [--since <seq>]` | Replay the Computer's journal, and follow it. |
| `a12s gateway [--web dir] [--port 7012] [--allow actions] [--computers c,d]` | Serve a web app (default: a built-in dashboard) on `127.0.0.1`, with the browser SDK at `/a12s.js` and the API at `/v1/` under a scoped token the page never sees. See [SDK: In the browser](sdk.md#in-the-browser). |
| `a12s shell <name> [--root] [-- command...]` | An audited terminal inside a Computer or Service, outside the harness. Counts as activity for the power policy. |

## Inbox

| Command | What it does |
|---|---|
| `a12s inbox <name> [--all] [--bodies]` | List pending Messages with limits, usage and the last 24 hours' expired, replaced, withdrawn and discarded counts. |
| `a12s inbox rm <name> <id>` | Withdraw a pending Message. |
| `a12s inbox clear <name> [--yes] [--orphan]` | Discard every pending Message (journaled, not erased). `--orphan` targets the inbox of a Computer no longer in the Fleet. |
| `a12s inbox hold <name>` | Keep accepting Messages, stop delivering them. |
| `a12s inbox resume <name> [--new]` | Deliver again; `--new` delivers the next Batch in a new session. |

## Power

| Command | What it does |
|---|---|
| `a12s sleep <name>... \| --all [--allow-wake]` | Stop processes, keep storage. A manual sleep suppresses automatic wakes until a manual wake; `--allow-wake` leaves the power policy in charge. |
| `a12s wake <name>... \| --all [--clear-inbox]` | Start Computers (or Services). The session resumes on the next Turn. `--clear-inbox` discards what was pending first. |

`--all` covers Services too.

## Services

| Command | What it does |
|---|---|
| `a12s service ls` | Name, phase, image and pin, grants, users, published ports, restarts. |
| `a12s service status <svc>` | One Service in detail. |
| `a12s service logs <svc> [-f] [--tail N]` | Container output plus the Controller's audit lines for that Service. |
| `a12s service login <svc>` | Run the Service's one-time interactive login. See [Services](services.md#credentials-and-the-one-time-login). |

## Credentials, tokens and drives

| Command | What it does |
|---|---|
| `a12s creds set <name>` | Read a value from stdin (not echoed), store it at `~/.a12s/<fleet>/<name>` (0600) and bind it. |
| `a12s token create <name> --actions a,b --computers c,d` | Create a scoped API token; it's printed once. Actions: `send`, `inbox`, `status`, `events`, `power`, `stop`. |
| `a12s token ls` / `a12s token rm <name>` | List or revoke tokens. |
| `a12s drive ls` | Drives, sizes, what's attached, orphans. |
| `a12s drive rm <name> [--yes]` | Destroy one drive (asks first). With `down --purge`, the only commands that destroy data. |

## Other

| Command | What it does |
|---|---|
| `a12s completion bash\|zsh` | Print a shell completion script. |
| `a12s controller` | Run the Controller. Used inside the Controller container; you don't run it yourself. |

## Example session

```text
$ a12s up
created scout, builder, tester, reviewer
$ a12s prompt builder "add a health check endpoint and open a PR"
turn t_41 started (codex 0.154.0, resumed: false)
… streamed text and tool events …
turn t_41 completed
$ a12s sleep --all
$ a12s status
```
