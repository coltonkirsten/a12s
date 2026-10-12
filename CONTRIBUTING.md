# Contributing

## Requirements

- Go 1.26+
- Node 24 and npm (for computerd)
- Docker (Docker Desktop on macOS, or Docker Engine on Linux) for images, `make check-full` and end-to-end runs
- uv for the Python SDK; jq for the bundle conformance check; GoReleaser (`brew install goreleaser`) only to release

## Run from source (macOS)

Needs Go 1.26+, Node 24, Docker Desktop running, and a `claude setup-token` token.

```sh
gh repo clone coltonkirsten/a12s && cd a12s
make build                     # go build; also writes bin/a12s (version dev: uses the local images below)
make images bundles            # slim, browser and controller images + Claude Code and Codex bundles

cd examples/quickstart
cp a12s.bindings.yaml.example a12s.bindings.yaml   # anthropic -> ~/.a12s/anthropic
../../bin/a12s up
../../bin/a12s prompt hello "Reply with the single word pong."
../../bin/a12s down --purge
```

## Build and test

```sh
make build        # go build ./... and bin/a12s
make test         # go test ./...
make vet          # go vet ./...
make fmt          # go fmt ./...

cd computerd
npm ci
npm run lint      # type-check
npm test          # build, then node --test
```

The client SDK in `sdk/typescript` builds and tests the same way; `npm test` also bundles its root entry with esbuild
`--platform=browser`, so a `node:` import there fails. Its types in `src/types.ts` are hand-written; to keep them in
step with `internal/api`, `go test ./internal/api` renders every API type into `sdk/typescript/test/api-fixtures.gen.ts`
and fails if the file is stale. After changing an API type, run `go test ./internal/api -run TestSDKFixtures -update`
and fix `src/types.ts` until the SDK's `npm run lint` passes.

The Python SDK in `sdk/python` parses the same fixtures strictly into its dataclasses:
`cd sdk/python && uv run --extra dev pytest -q && uv run --extra dev mypy --strict a12s`.

Images:

```sh
make images       # a12s/slim:dev, a12s/browser:dev, a12s/controller:dev
make bundles      # a12s/bundle-claude-code:<v>, a12s/bundle-codex:<v>
                  # (versions: BUNDLE_CLAUDE_VERSIONS, BUNDLE_CODEX_VERSIONS)
make services     # the reference Services: svc-cron, svc-webhook, svc-mcp, svc-mcp-postgres
```

## Checks: `make check` and `make check-full`

```sh
make check        # what PR CI runs: go, sdk, python (about 1.5 minutes, no Docker)
make check-full   # everything; stops at the first failure (about 15 minutes cold, needs Docker)
scripts/check.sh go sdk    # any subset of: go computerd sdk python services tiers controller bundles
```

`make check` runs `go build`, `gofmt`, `go vet` and `go test` (which includes the Go to TypeScript/Python API fixture
drift check), the TypeScript SDK's lint and tests (including the browser bundle) and the check that
`internal/gateway/web/a12s.js` matches its build, and the Python SDK's tests and `mypy --strict` on Python 3.10 and
3.14 (`CHECK_PYTHONS` overrides).

`make check-full` adds computerd's lint, build and tests, svc-mcp against a stub server, the reference Service
images, the slim, browser and controller images with the Chromium sandbox and Playwright checks, and both default
harness bundles with `images/bundles/conformance.sh`. Afterwards it prunes dangling images and the build cache
(`CHECK_KEEP_CACHE=1` keeps them).

**PR CI is light on purpose.** `.github/workflows/ci.yml` runs the `make check` steps (one job each for go, sdk and
python) on every pull request to main, once per update, cancelling superseded runs and skipping docs-only changes
(`**/*.md`, `docs/**`). Nothing Docker-heavy runs on GitHub for a PR: run `make check-full` locally before any
release-affecting change (images and Dockerfiles, computerd, Services, bundles, `scripts/release.sh`,
`.goreleaser.yaml`) and paste its tail in the PR.

## Releasing

Push a tag; `.github/workflows/release.yml` does the rest on GitHub's runners:

```sh
git tag -a v0.5.0 -m v0.5.0 origin/main && git push origin v0.5.0
```

It pushes the images (linux/amd64 and linux/arm64), publishes the default harness bundles if missing, runs
GoReleaser (binaries and the Homebrew formula), attaches the SDK `a12s-*.tgz` and `a12s-*.whl` to the GitHub release
and publishes `a12s` to npm and PyPI. For an existing tag, start it by hand (Actions > release > Run workflow).
`make check-full` should be green on that commit first.

### npm and PyPI: trusted publishing

The workflow publishes with trusted publishing (OIDC; no long-lived token in the repo). It tries trusted publishing
first, then the `NPM_TOKEN` / `PYPI_TOKEN` repository secret if one is set, and otherwise skips that registry with a
`::warning::` (the rest of the release still succeeds). One-time setup, each with owner `coltonkirsten`, repository
`a12s`, workflow `release.yml`, no environment:

- PyPI: pypi.org > Your account > Publishing (https://pypi.org/manage/account/publishing/) > add a pending GitHub
  publisher for the project name `a12s`. The first release creates the project. A pending publisher does not reserve
  the name until that first publish.
- npm: a trusted publisher can only be added to a package that exists, so publish once by hand, then configure it:
  `gh release download v0.2.0 -R coltonkirsten/a12s -p 'a12s-*.tgz' && npm login && npm publish a12s-0.2.0.tgz
  --access public`, then npmjs.com > package `a12s` > Settings > Trusted Publisher > GitHub Actions. Optionally also
  set "Require two-factor authentication and disallow tokens" there afterwards. Needs npm 11.5.1+ in the workflow
  (it upgrades npm when the runner's is older).

### Local fallback: `make release`

From a Mac or Linux machine with Docker, on an up-to-date main that passed `make check-full`:

```sh
make release VERSION=0.5.0
```

`scripts/release.sh` builds from a clean checkout of `origin/main`. It pushes the slim, browser, controller and
reference Service images to `ghcr.io/coltonkirsten/a12s/<name>:<version>` (linux/amd64 and linux/arm64), publishes the
default harness bundles if missing, tags `v<version>` and pushes the tag, then runs GoReleaser (`.goreleaser.yaml`):
binaries for darwin and linux on amd64 and arm64, built with the release version and registry, and the formula in
`coltonkirsten/homebrew-tap`. It attaches the SDK `a12s-*.tgz` and `a12s-*.whl` to the GitHub release, and publishes
the `a12s` package to npm if `NPM_TOKEN` is set and to PyPI if `PYPI_TOKEN` is set; without them it skips with a
warning.

It checks everything it needs before touching anything and lists what is missing:

- `gh` logged in with a token that can push packages. If it says the token cannot push to ghcr.io:
  `gh auth refresh -s write:packages,read:packages`.
- `goreleaser` on the PATH.
- `HOMEBREW_TAP_DEPLOY_KEY`: a path to (or the contents of) an SSH key with write access to the tap. Defaults to
  `~/.ssh/id_ed25519` or `~/.ssh/id_rsa`; it must have no passphrase, since GoReleaser reads it directly.

Its tag carries the line `released-by: make release`, so the tag push does not start a second release on GitHub.
Trusted publishing works only from the workflow, so locally npm and PyPI need `NPM_TOKEN` / `PYPI_TOKEN`.

## Repository layout

```text
internal/version/          the release version and the image refs it resolves (set by -ldflags)
cmd/a12s/                  the CLI and, as `a12s controller`, the Controller (one Go binary)
internal/fleet/            Fleet file: parse, defaults merge, validation, fleet.lock, Service manifests
internal/driver/           Driver interface and types
internal/driver/docker/    Docker driver: containers, internal networks, volumes, bundles
internal/controller/       reconciler, power, CA and mTLS, credential injection, egress proxy, owner API
internal/inbox/            inbox store (fleet.db), delivery, Batch rendering, tokens, certificate registry
internal/api/              CLI ↔ Controller API types (JSON)
internal/cli/              CLI helpers: client, bindings, creds, SSE, output formatting
internal/computerd/        Go client for computerd's HTTP+SSE API
internal/serviceinit/      a12s-init: the Service entrypoint, privilege drop and TCP forwarders
internal/sse/              SSE reader and writer
sdk/typescript/            the `a12s` npm package: the TypeScript client apps use
sdk/python/                the `a12s` PyPI package: the Python client, sync and asyncio
computerd/                 TypeScript daemon inside every Computer: server, journal, adapters, system tools, MCP bridge
images/slim, images/browser, images/controller   container images
images/bundles/            HarnessBundle images and conformance.sh
services/                  reference Services (svc-cron, svc-mcp, svc-webhook) and svckit, their shared Go library
examples/                  runnable Fleets: quickstart, lab, services
hack/                      end-to-end scripts
```

## computerd on a host

computerd can run outside a container for development. When it isn't root it skips chown and uid switching, and
`A12S_HOME_ROOT` (default `/home`), `A12S_RUN_DIR` (default `/run/a12s`) and `A12S_PORT` (default 7443) can point
elsewhere. Put `ca.crt`, `cert.pem` and `key.pem` in `$A12S_RUN_DIR/tls/`, secrets in `$A12S_RUN_DIR/secrets/<name>`,
and a bundle directory with `bin/claude` or `bin/codex` somewhere, then:

```sh
A12S_COMPUTER=c A12S_HARNESS_KIND=codex A12S_BUNDLE=… A12S_HOME_ROOT=… A12S_RUN_DIR=… node dist/main.js
```

A smoke run against a real harness (temporary directories, a throwaway openssl CA, credentials copied in and deleted
afterwards) covers prompt, resume, stop, `restart_harness`, and restart plus resume:

```sh
npm run build && node scripts/smoke.mjs claude-code     # or: node scripts/smoke.mjs codex
```

Override `CLAUDE_BIN`, `CODEX_BIN` and `A12S_LAB_CREDS` as needed.

## End-to-end checks

Two scripts run the whole system against the local Docker engine and log one `PASS`/`FAIL` line per check:

```sh
hack/exit-gate.sh <logfile>
#   examples/lab: four Computers up, streamed replies, resume after sleep/wake, scaling, SSE reconnect,
#   blocked egress, harness version bump. Needs bin/a12s, the images and bundles,
#   examples/lab/a12s.bindings.yaml and GH_TOKEN.

hack/exit-gate-enhancements.sh <workdir> <logfile> [setup inbox services amendment cli teardown]
#   examples/services: inbox delivery and crash safety, Services, grants, login, expiry, replace keys,
#   power policy, CLI. Needs bin/a12s, the images, the claude-code bundle, `make services` and ~/.a12s/anthropic.
```

Both use real model calls. `A12S_TEST_TIME_SCALE` (≥ 1) divides `sleepAfterIdle` and the wake rate window so the
power-policy checks run in seconds; never set it in production (the Controller logs when it is in force).

## Pull requests

- Keep changes small and focused, with tests for behavior changes.
- PR CI (go, sdk, python) green before merge; `make check` runs the same locally. For release-affecting changes also
  run `make check-full` and paste the tail of its output in the PR.
- Update the docs in `docs/` when behavior changes: they describe the current state of the code.
