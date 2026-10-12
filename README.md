# a12s — agent computers

a12s runs AI coding agents (Claude Code, Codex) as durable, isolated computers on Docker. One file declares them,
one command starts them, and your app sends them work and watches them do it.

```text
fleet.yaml ──a12s up──▶  lead   worker1   worker2      each a Computer: its own disk, session, inbox and network rules
                           ▲  └──send──▶   │    │      agents hand work to each other through their inboxes
your app (TS, Python, ─────┘ ◀──events─────┴────┘      your app sends work and follows every Turn as it streams
browser, curl)
```

- **Computers keep their state.** Each has a home drive, a conversation (session) that survives sleep, and an inbox
  that takes work while it is busy or asleep. Containers come and go; the Computer stays.
- **Safe by default.** Credentials live only in memory, agents run as non-root on a read-only root filesystem, and
  all traffic goes through an allowlisting proxy.
- **Built for apps.** SDKs for TypeScript and Python, a browser gateway, an HTTP API, and a template repo.

## Quickstart (5 minutes)

Needs Docker running and a Claude subscription or API access. The repo is private, so brew needs a GitHub token
with `repo` scope:

```sh
export HOMEBREW_GITHUB_API_TOKEN=<token>
brew install coltonkirsten/tap/a12s

a12s init hello && cd hello        # fleet.yaml with one Computer, "hello"
claude setup-token                 # prints a long-lived sk-ant-oat… token
a12s creds set anthropic           # paste it; stored 0600 under ~/.a12s
a12s up                            # first run pulls the images (about 1.5 GB)
a12s prompt hello "Reply with the single word pong."
```

`prompt` prints `turn … started`, `pong`, `turn … completed`. Then:

```sh
a12s gateway                       # open http://127.0.0.1:7012: every Computer, live, with a send box
a12s down                          # stop everything; disks and conversations are kept
```

## Build a multi-agent app

Start from the template: a lead that splits a task between two workers, an app in TypeScript and in Python, and a
live browser view.

```sh
gh repo create my-agents --private --template coltonkirsten/a12s-starter --clone && cd my-agents
make demo                          # bind credentials, a12s up, run one task end to end
```

Or add the SDK to your own app:

```ts
import { connect } from "a12s";                      // npm install a12s

const a12s = await connect();                        // finds the running Fleet
const turn = await a12s.prompt("hello", "Name three prime numbers.");
console.log(turn.text);
```

```python
import a12s                                          # pip install a12s

client = a12s.connect()
turn = client.prompt("hello", "Name three prime numbers.")
print(turn.text)
```

Until the packages are on npm and PyPI, install them from the release:
`gh release download -R coltonkirsten/a12s -p 'a12s-*'`, then `npm install ./a12s-<version>.tgz` or
`pip install ./a12s-<version>-py3-none-any.whl`.

## Learn it

Read these in order; each takes a few minutes.

1. [Concepts](docs/concepts.md): the five ideas you need (Fleet, Computer, Message, Turn, Event) and how they fit.
2. [Recipes](docs/recipes.md): lead and workers, a live dashboard, changing an agent's instructions, schedules,
   webhooks, tools and network access.
3. [SDK](docs/sdk.md): TypeScript and Python, side by side.

Reference: [Fleet file](docs/fleet-file.md) · [CLI](docs/cli.md) · [HTTP API](docs/api.md) ·
[Inbox and power](docs/inbox.md) · [Services](docs/services.md) · [Credentials](docs/credentials.md) ·
[Extending](docs/extending.md) · [Images](docs/images.md) · [Security](docs/security.md) ·
[Architecture](docs/architecture.md)

Coding agents: [`llms.txt`](llms.txt) is the whole thing on one page, with exact commands.

## Status

a12s is early (`a12s.ai/v1alpha1`, see [CHANGELOG](CHANGELOG.md)). It runs on Docker: Docker Desktop on macOS
(arm64) and Docker Engine on Linux (amd64, arm64). Fleet files and the API may still change before 1.0; the
`--json` output is the stable contract.

Building a12s itself: [CONTRIBUTING.md](CONTRIBUTING.md). MIT licensed.
