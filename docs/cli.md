# CLI reference

```text
a12s [--fleet|-F fleet.yaml] [--json] <command> [args]
```

| Global flag | Meaning |
|---|---|
| `--fleet`, `-F` | The Fleet file. Default: `$A12S_FLEET`, then the nearest `fleet.yaml` upward. |
| `--json` | Machine-readable output. Listings print JSON; `logs` and `send --wait` print events as JSON lines. The JSON is the stable contract; human text may change. |

Exit codes: `0` success, `1` failure, `130` interrupted. Errors end with a `fix:` that says what to do.

## Fleet

| Command | What it does |
|---|---|
| `a12s init [dir] [--harness claude-code\|codex]` | Scaffold `fleet.yaml` (harness, `image: slim` and the credential in `defaults`, and one agent computer, `assistant`) and a README. |
| `a12s up [--controller-image ref]` | Start the Controller if needed, validate the Fleet file and make the live Fleet match it, without asking: create, update or stage agent computers and Services, and remove the ones the file doesn't list, including those created through the SDK or API (drives are kept). It reads each `security.seccomp` file and sends the profile. It sends no credentials: an agent computer or Service whose credential isn't set on the Controller waits in Provisioning, and `up` prints a warning with the fix. It prints `<name> runs with its own seccomp profile` for each agent computer with [`security.seccomp`](security.md#seccomp). It resolves each [`attach`](fleet-file.md#attached-folders) host to the folder's absolute path (failing if it isn't a folder) and prints `<name> has <folder> at /home/agent/<name> (read-write)` for each. |
| `a12s export` | Write the live Fleet back to the Fleet file's directory. See [Keeping changes made from code](#keeping-changes-made-from-code). |
| `a12s status [<name>]` | Phase, session, active Turn, harness version, inbox, power policy, `seccomp sha256:…` for a custom seccomp profile, and warnings (orphaned drives, `allow: ["*"]`). |
| `a12s down [--purge [--yes]]` | Remove the Fleet's containers, networks and Controller; keep drives and the inbox database. `--purge` also destroys them after listing them and asking. Credential values the Controller stored go with its state volume. |
| `a12s doctor` | Check the Fleet file, Docker, images and bundles, the Controller, egress and every agent computer's and Service's health. One line per check with the fix; exits 1 on any failure. |
| `a12s version` | The a12s release and the registry its images come from. |

### Keeping changes made from code

The live Fleet is the source of truth; `fleet.yaml` is optional. `a12s up` undoes changes made through the SDK or
API that the file doesn't have, so to keep them run `a12s export` first. It writes the live Fleet to `fleet.yaml` in
the Fleet file's directory:

- Instructions and files stay in `fleet.yaml`, multi-line ones as YAML block scalars.
- A seccomp profile goes to `profiles/<name>.json`.
- An `attach` host is written relative to `fleet.yaml` when the folder is inside its directory or next to it, else as
  `~/…` or absolute.
- Credential values are never written; the Fleet names credentials only.
- It overwrites files that differ. Comments in `fleet.yaml` are lost.

`git diff` shows what changed. There is no CLI preview; the API has [`?dryRun=true`](api.md#changing-the-fleet).

## Working with agent computers

| Command | What it does |
|---|---|
| `a12s send <name> "…" [flags]` | Enqueue a Message and exit; works while the agent computer is busy or asleep. `-` reads the body from stdin. Flags: `--from`, `--fresh`, `--meta k=v`, `--expires`, `--replace` (see [Inbox](inbox.md#message-options)). With `--wait` it then streams the Turn that delivers the Message and exits `0` if that Turn completed, `1` if not, `130` if interrupted (Ctrl-C withdraws a pending Message or stops its Turn); an agent computer that is asleep and won't wake for you fails at once. With `--json`, `--wait` prints `{message, turn}`, then the Turn's events as JSON lines. |
| `a12s stop <name>` | Interrupt the active Turn. To deliver nothing after it, `a12s inbox hold <name>` first. |
| `a12s logs <name> [--follow]` | Replay the agent computer's journal, and follow it. |
| `a12s gateway --web dir \| --dev url \| -- command... [--dev-port n] [--port 7012 \| --addr host:port] [--allow actions] [--agent-computers c,d] [--no-open]` | Serve your web app on `127.0.0.1`: a built app's directory (`--web dist`), a dev server it starts (`-- npm run dev`; it reads the port from the dev server's output, or `--dev-port`) or one already running (`--dev`), proxied with websockets; one of them is required (the dashboard is `a12s ui`). Next to it, the API at `/v1/` under a scoped token the page never sees; the page imports `@coltonkirsten/a12s` through a bundler. It opens `http://127.0.0.1:<port>/?key=…` in the browser (`--no-open`: only print it); the key is new each run, and `/v1/` needs it. Like every command it serves the Fleet found from `-F` or the current directory; if the port is taken it says so, and `--port` picks another. Each gateway gets its own scoped token, `gateway-<port>`, revoked when it stops (a killed gateway's token is removed by the next one to start), so several can serve one Fleet on different ports. `--addr` takes a loopback address only; anything else is refused (see [SDK: Serve other people](sdk.md#serve-other-people)). See [SDK: In the browser](sdk.md#in-the-browser). |
| `a12s ui [console args...]` | Open the web console: runs `npx --yes github:coltonkirsten/a12s-console --fleet <fleet> [args]` (`A12S_CONSOLE` names another package or path), for the Fleet found from `-F` (before `ui`) or the current directory. Every argument after `ui` goes to the console, e.g. `--port 7013 --no-open`. Needs Node.js 20+ (`npx`) and a running Fleet; exits with the console's exit code. |
| `a12s shell <name> [-- command...]` | An audited terminal inside an agent computer or Service, outside the harness. Counts as activity for the power policy. |

## Inbox

| Command | What it does |
|---|---|
| `a12s inbox <name> [--all] [--bodies]` | List pending Messages with limits, usage and the last 24 hours' expired, replaced, withdrawn and discarded counts. |
| `a12s inbox rm <name> <id>` | Withdraw a pending Message. |
| `a12s inbox clear <name> [--yes]` | Discard every pending Message (journaled, not erased), also in the inbox of an agent computer no longer in the Fleet. |
| `a12s inbox hold <name>` | Keep accepting Messages, stop delivering them. |
| `a12s inbox resume <name> [--new]` | Deliver again; `--new` delivers the next Batch in a new session. |

## Power

| Command | What it does |
|---|---|
| `a12s sleep <name>... \| --all [--hold]` | Stop processes, keep storage. The power policy stays in charge: a sender listed in `wakeOnMessage` wakes it; `--hold` keeps it asleep until a manual wake. |
| `a12s wake <name>... \| --all` | Start agent computers (or Services). The session resumes on the next Turn. To drop what is pending first, `a12s inbox clear <name>`. |

`--all` covers Services too.

## Services

| Command | What it does |
|---|---|
| `a12s service ls` | Name, phase, image and pin, grants, users, restarts. |
| `a12s service status <svc>` | One Service in detail, including the image ID its ref is pinned to. |
| `a12s service logs <svc> [-f] [--tail N]` | Container output plus the Controller's audit lines for that Service. |

## Credentials, tokens and drives

| Command | What it does |
|---|---|
| `a12s creds set <name>` | Read a value from stdin (not echoed) and set it on the Fleet's Controller, starting the Controller if none runs; agent computers and Services that use it get it at once. Nothing is stored on the host. Before or after `a12s up`, either order works. Run it in the Fleet's directory (or with `-F`). For `anthropic` it refuses a value that isn't one unbroken `sk-ant-oat01-…` or `sk-ant-api03-…` string of about 108 characters and prints the fix; `--no-check` skips that. |
| `a12s token create <name> --prefix biz-` | Create a prefix token, printed once: it may do everything the owner can for agent computers, Services and credentials whose names start with `biz-` except set a seccomp profile or attach a host folder, and nothing outside. See [Security: Tokens](security.md#tokens). |
| `a12s token create <name> --actions a,b --agent-computers c,d` | Create a narrow token for those agent computers (or `'*'`), printed once. Actions: `send`, `inbox`, `status`, `events`, `power`, `stop`. |
| `a12s token ls` / `a12s token rm <name>` | List or revoke tokens. |
| `a12s drive ls` | Home drives (`home-<name>`), the drives of [kept paths](fleet-file.md#attached-folders) (`keep-…`) and Service state drives (`<svc>-state`): what's attached, orphans. |
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
$ a12s send builder "add a health check endpoint and open a PR" --wait
turn t_41 started (codex 0.154.0, resumed: false)
… streamed text and tool events …
turn t_41 completed
$ a12s sleep --all
$ a12s status
```
