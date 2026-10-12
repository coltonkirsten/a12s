# a12s — agent computers

a12s ("agent computers", like k8s) runs a fleet of AI coding agents as durable, isolated computers on Docker.

One file in git declares several agent computers. One command runs them. Prompt any of them and watch the reply
stream back. Put them to sleep and they cost nothing; wake them and they pick up the same conversation.

- **Two harnesses:** Claude Code and Codex, each running its own native session, which a12s resumes after sleep.
- **Durable:** each Computer has a home drive, optional work drives, an append-only event journal and an inbox.
  A container can be replaced; the Computer stays.
- **Secure by default:** credentials live only in tmpfs, agents run as non-root on a read-only root filesystem, and
  every Computer reaches the network only through an allowlisting egress proxy.
- **Inbox:** send work to a Computer while it's busy or asleep; a declared power policy can sleep it when idle and
  wake it on a message.
- **Services:** non-agent containers (a clock, a database, an MCP server that holds a login, a webhook bridge),
  declared in the same file, with least-privilege grants.
- **No image rebuilds for upgrades:** the harness ships as a pinned, read-only bundle. Bumping Claude Code is a
  one-line edit.

```text
a12s up                      # apply fleet.yaml: create, update or stage Computers
a12s prompt scout "hello"    # send a turn, stream its events
a12s send scout "later"      # queue work and exit (works while asleep)
a12s sleep --all             # stop processes, keep disks
a12s wake --all              # resume happens on the next prompt
a12s status                  # power, session, inbox, warnings
a12s down                    # remove Computers and Controller, keep drives
```

## Install

a12s is a tool you install, like Docker. A project pins a release in its `fleet.lock`; it never vendors or edits
a12s source. The repo, release assets and images are private, so installing needs a GitHub token with `repo` (to
download the release) and `read:packages` (to pull images from ghcr.io):

```sh
export HOMEBREW_GITHUB_API_TOKEN=<token>          # brew downloads the private release asset with it
brew tap coltonkirsten/tap https://github.com/coltonkirsten/homebrew-tap   # git needs access to the private tap
brew install coltonkirsten/tap/a12s
echo <token> | docker login ghcr.io -u <github-user> --password-stdin      # images are pulled on the host
a12s version                                       # a12s v0.1.0 (images from ghcr.io/coltonkirsten/a12s)
```

A released binary resolves every image by its own version: `slim` is `ghcr.io/coltonkirsten/a12s/slim:<version>`,
the Controller is `…/controller:<version>`, and `a12s init` writes the reference Services as `…/svc-cron:<version>`.
Harness bundles are versioned by the harness (`…/bundle-claude-code:2.1.280`). `a12s up` pulls missing images with
your `docker login`. A source build (`make build`) is version `dev` and uses the local images from `make images`.

```sh
a12s init myproject && cd myproject   # fleet.yaml, fleet.lock (pins the a12s release), .gitignore, README
a12s creds set anthropic              # paste a `claude setup-token` token
a12s up && a12s prompt hello "Reply with the single word pong."
a12s down
```

To build on a12s without forking it (your own Services, agent images, harness versions, credentials, drives and
egress, and the TypeScript client), see [Extending a12s](docs/extending.md).

## Quickstart from source (macOS)

Needs Go 1.26+, Docker Desktop running, and Claude Code signed in to your Claude account.

```sh
gh repo clone coltonkirsten/a12s && cd a12s
make build                     # go build; also writes bin/a12s
make images bundles            # slim, browser and controller images + Claude Code and Codex bundles (a few minutes the first time)

claude setup-token             # sign in in the browser; copy the sk-ant-oat… token it prints
mkdir -p ~/.a12s && pbpaste > ~/.a12s/anthropic && chmod 600 ~/.a12s/anthropic

cd examples/quickstart
cp a12s.bindings.yaml.example a12s.bindings.yaml   # anthropic -> ~/.a12s/anthropic
../../bin/a12s up                                  # starts the Controller, creates the Computers
../../bin/a12s prompt hello "Reply with the single word pong."
../../bin/a12s sleep --all                         # stops them; disks and sessions are kept
```

`prompt` prints `turn … started`, then `pong`, then `turn … completed`. An `[diagnostic] egress denied` line is
normal: Claude Code also tries telemetry hosts that the allowlist blocks.

Your Claude token reaches the Computer only through the bindings file: it is injected into tmpfs at start and never
written to the image or a disk. For Codex, bind `openai` to an OpenAI API key or a copy of `~/.codex/auth.json`
(see [Credentials](docs/credentials.md)). To delete the quickstart Fleet and its disks:

```sh
../../bin/a12s down --purge     # lists the drives and asks; plain `a12s down` keeps them
```

### Start your own Fleet

```sh
a12s init demo --with cron && cd demo     # scaffold fleet.yaml, fleet.lock, bindings, .gitignore, plus a clock Service
a12s creds set anthropic                  # paste a `claude setup-token` token
make -C .. services && a12s up            # build the reference Services, then start everything
a12s doctor                               # one line per check, with the fix
```

## Examples

| Directory | What it shows |
|---|---|
| [`examples/quickstart`](examples/quickstart) | The smallest Fleet: Claude Code on the slim tier. |
| [`examples/lab`](examples/lab) | Four Computers: Claude Code and Codex, slim and browser tiers, shared and exclusive drives, a home preset. |
| [`examples/services`](examples/services) | Three Computers plus every reference Service: cron, Postgres behind an MCP server, a webhook bridge. |

## Documentation

- [Concepts](docs/concepts.md): Fleet, Computer, Drive, Controller, computerd, Session, Turn, inbox, Service
- [Fleet file reference](docs/fleet-file.md): `fleet.yaml`, the bindings file and `fleet.lock`
- [CLI reference](docs/cli.md): every command and flag
- [Inbox and power policy](docs/inbox.md): queuing, delivery, expiry, replace keys, auto-sleep and wake
- [Services](docs/services.md): non-agent containers, grants, the manifest, one-time login
- [Credentials](docs/credentials.md): what to bind for Claude Code, Codex and GitHub, and how secrets are handled
- [Security model](docs/security.md): isolation, egress, identity and honest limits
- [Architecture](docs/architecture.md): the Controller, the Docker driver, computerd, the APIs and the journal
- [Images and harness bundles](docs/images.md): the slim and browser tiers, bundles, the Chromium sandbox
- [Extending a12s](docs/extending.md): custom Services and agent images, the client SDK, without forking

## Status

a12s is early (`a12s.ai/v1alpha1`). It runs on Docker: end-to-end runs use Docker Desktop on macOS (arm64), CI
builds on Linux (amd64), and bundles are published for both architectures. Fleet files, the CLI's text output and
the API may still change; the JSON output (`--json`) is the stable contract.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for building, testing and the repository layout.

## License

[MIT](LICENSE)
