# CLI reference

```text
a12s [--fleet|-F fleet.yaml] [--bindings path] [--json] <command> [args]
```

| Global flag | Meaning |
|---|---|
| `--fleet`, `-F` | The Fleet file. Default: `$A12S_FLEET`, then the nearest `fleet.yaml` upward. |
| `--bindings` | The bindings file. Default: `a12s.bindings.yaml` next to the Fleet file. |
| `--json` | Machine-readable output. Listings print JSON; `logs` and `send --wait` print events as JSON lines. The JSON is the stable contract; human text may change. |

Exit codes: `0` success, `1` failure, `2` from `a12s diff` when the file and the live Fleet differ, `130`
interrupted. Errors end with a `fix:` that says what to do.

## Fleet

| Command | What it does |
|---|---|
| `a12s init [dir] [--harness claude-code\|codex]` | Scaffold `fleet.yaml` (harness, `image: slim` and the credential in `defaults`, and one agent computer, `assistant`), `fleet.lock` (pinning this a12s release), an empty bindings file, `.gitignore` and a README. |
| `a12s up [--asleep] [--force] [--memory-only-credentials] [--controller-image ref]` | Start the Controller if needed, validate and apply the Fleet file: create, update or stage agent computers and Services, remove the ones the file doesn't list (including those created through the SDK or API), pin new digests in `fleet.lock`. It reads each `home.preset` directory and `security.seccomp` file and sends their contents, and sends every credential bound in the bindings file (an agent computer or Service whose credential is set nowhere is created and waits, and `up` prints a warning with the fix). It prints `<name> runs with its own seccomp profile` for each agent computer with [`security.seccomp`](security.md#seccomp), and a hint when the file still uses the old `computers:` key. `--asleep` creates new agent computers asleep. `--memory-only-credentials` makes a Controller that this `up` starts keep credential values in memory only ([Credentials](credentials.md#how-secrets-are-handled)). See [Changes made through the API](#changes-made-through-the-api) for `--force`. |
| `a12s diff` | Compare the Fleet file with the live Fleet and change nothing (a dry run of `up`): prints one line per change (`+ create agent computer <name>`, `~ update Service <name>: <fields>`, `- remove …`). Exits `2` if they differ, `0` if not. `--json` prints the change list. |
| `a12s export [--force]` | Write the live Fleet back to the Fleet file's directory. See [Changes made through the API](#changes-made-through-the-api). |
| `a12s apply --interrupt [--grace 30s]` | Apply staged changes to busy agent computers now, interrupting their Turns after the grace period. |
| `a12s status [<name>]` | Phase, session, active Turn, harness version, inbox, power policy, `seccomp sha256:…` for a custom seccomp profile, and warnings (orphaned drives, unenforced egress, `allow: ["*"]`, published ports). |
| `a12s down [--purge [--yes]]` | Remove the Fleet's containers, networks and Controller; keep drives and the inbox database. `--purge` also destroys them after listing them and asking. Neither touches credentials saved with `creds set` (`~/.a12s/<fleet>/`); the values the Controller stored go with its state volume. |
| `a12s doctor` | Check the Fleet file, Docker, images and bundles, bindings, the Controller, egress and every agent computer's and Service's health. One line per check with the fix; exits 1 on any failure. |
| `a12s lock update services/<svc>` | Re-pin a Service whose image ref hasn't changed. |
| `a12s version` | The a12s release and the registry its images come from. |

### Changes made through the API

Agent computers and Services changed through the SDK or API since the last `a12s up` are drift (`GET /v1/fleet`
lists them): the last `PUT /v1/fleet` sent with `file: true`, as `a12s up` does, is the baseline, so a
`fleet.replace` write counts as drift too. When `a12s up` would undo drift, it lists those changes and asks first:

```text
$ a12s up
the live Fleet changed since the last a12s up; this up undoes:
  - remove agent computer review-7
  ~ update agent computer lead: instructions
apply anyway? [y/N]
```

Without a terminal (CI) it fails instead, unless `--force`. The write is conditional on the Fleet version `up`
read, so a change that lands in between fails it with `version_mismatch`; run it again.

To keep the changes, `a12s export` writes the live Fleet back to `fleet.yaml` in the Fleet file's directory:

- Instructions of up to 2000 characters, with no `files`, stay inline. Longer instructions and any files go to
  `presets/<name>/` (the instructions as `CLAUDE.md`, or `AGENTS.md` for Codex) and the agent computer gets
  `home.preset: ./presets/<name>`; `defaults` use `presets/defaults/`.
- A seccomp profile goes to `profiles/<name>.json`.
- Credential values are never written; the Fleet names credentials only.
- It refuses to overwrite a file that differs, or to remove a file in one of those preset directories that the live
  Fleet doesn't have, unless `--force`. Comments in an existing `fleet.yaml` are lost when it is overwritten.

Commit the result, check it with `a12s diff`, and run `a12s up`: the file it sends becomes the new baseline.

## Working with agent computers

| Command | What it does |
|---|---|
| `a12s send <name> "…" [flags]` | Enqueue a Message and exit; works while the agent computer is busy or asleep. `-` reads the body from stdin. Flags: `--from`, `--fresh`, `--meta k=v`, `--key`, `--expires`, `--replace` (see [Inbox](inbox.md#message-options)). With `--wait` it then streams the Turn that delivers the Message and exits `0` if that Turn completed, `1` if not, `130` if interrupted (Ctrl-C withdraws a pending Message or stops its Turn); an agent computer that is asleep and won't wake for you fails at once. With `--json`, `--wait` prints `{message, turn}`, then the Turn's events as JSON lines. |
| `a12s stop <name> [--hold]` | Interrupt the active Turn. `--hold` holds the inbox first, so nothing else is delivered. |
| `a12s logs <name> [--follow] [--since <seq>]` | Replay the agent computer's journal, and follow it. |
| `a12s gateway [--web dir \| --dev url \| -- command...] [--dev-port n] [--port 7012 \| --addr host:port] [--allow actions] [--computers c,d] [--no-open]` | Serve a web app (default: a built-in dashboard) on `127.0.0.1`: a built app's directory (`--web dist`), a dev server it starts (`-- npm run dev`; it reads the port from the dev server's output, or `--dev-port`) or one already running (`--dev`), proxied with websockets. Next to it, the API at `/v1/` under a scoped token the page never sees; the page imports `@coltonkirsten/a12s` through a bundler. It opens `http://127.0.0.1:<port>/?key=…` in the browser (`--no-open`: only print it); the key is new each run, and `/v1/` needs it. Like every command it serves the Fleet found from `-F` or the current directory; if the port is taken it says so, and `--port` picks another. Each gateway gets its own scoped token, `gateway-<port>`, revoked when it stops (a killed gateway's token is removed by the next one to start), so several can serve one Fleet on different ports. `--addr` takes a loopback address only; anything else is refused (see [SDK: Serve other people](sdk.md#serve-other-people)). See [SDK: In the browser](sdk.md#in-the-browser). |
| `a12s ui [console args...]` | Open the web console: runs `npx --yes github:coltonkirsten/a12s-console --fleet <fleet> [args]` (`A12S_CONSOLE` names another package or path), for the Fleet found from `-F` (before `ui`) or the current directory. Every argument after `ui` goes to the console, e.g. `--port 7013 --no-open`. Needs Node.js 20+ (`npx`) and a running Fleet; exits with the console's exit code. |
| `a12s shell <name> [--root] [-- command...]` | An audited terminal inside an agent computer or Service, outside the harness. Counts as activity for the power policy. |

## Inbox

| Command | What it does |
|---|---|
| `a12s inbox <name> [--all] [--bodies]` | List pending Messages with limits, usage and the last 24 hours' expired, replaced, withdrawn and discarded counts. |
| `a12s inbox rm <name> <id>` | Withdraw a pending Message. |
| `a12s inbox clear <name> [--yes] [--orphan]` | Discard every pending Message (journaled, not erased). `--orphan` targets the inbox of an agent computer no longer in the Fleet. |
| `a12s inbox hold <name>` | Keep accepting Messages, stop delivering them. |
| `a12s inbox resume <name> [--new]` | Deliver again; `--new` delivers the next Batch in a new session. |

## Power

| Command | What it does |
|---|---|
| `a12s sleep <name>... \| --all [--hold]` | Stop processes, keep storage. The power policy stays in charge: a sender listed in `wakeOnMessage` wakes it; `--hold` keeps it asleep until a manual wake. |
| `a12s wake <name>... \| --all [--clear-inbox]` | Start agent computers (or Services). The session resumes on the next Turn. `--clear-inbox` discards what was pending first. |

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
| `a12s creds set <name>` | Read a value from stdin (not echoed), store it at `~/.a12s/<fleet>/<name>` (0600) and bind it in the Fleet's bindings file. If the Fleet's Controller is running, also set it there: agent computers and Services that use it get it at once. Otherwise the next `a12s up` sends it. Run it in the Fleet's directory (or with `-F`). `down --purge` keeps these files. For `anthropic` it refuses a value that isn't one unbroken `sk-ant-oat01-…` or `sk-ant-api03-…` string of about 108 characters and prints the fix; `--no-check` skips that. |
| `a12s token create <name> --prefix biz-` | Create a prefix token, printed once: it may do everything the owner can for agent computers, Services and credentials whose names start with `biz-`, and nothing outside. See [Security: Tokens](security.md#tokens). |
| `a12s token create <name> --actions a,b --agent-computers c,d` | Create a narrow token for those agent computers (or `'*'`), printed once. Actions: `send`, `inbox`, `status`, `events`, `power`, `stop`. `--computers` is the old name of `--agent-computers`. |
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
$ a12s send builder "add a health check endpoint and open a PR" --wait
turn t_41 started (codex 0.154.0, resumed: false)
… streamed text and tool events …
turn t_41 completed
$ a12s sleep --all
$ a12s status
```
