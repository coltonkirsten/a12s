# Contributing

## Requirements

- Go 1.26+
- Node 24 and npm (for computerd)
- Docker (Docker Desktop on macOS, or Docker Engine on Linux) for images and end-to-end runs

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

CI (`.github/workflows/ci.yml`) runs `go build`, `gofmt`, `go vet`, `go test`, computerd's lint, build and tests,
and builds the slim and browser images and checks the Chromium sandbox. It needs no secrets.

## Releasing

Push a `v*` tag. `.github/workflows/release.yml` pushes the slim, browser, controller and reference Service images to
`ghcr.io/coltonkirsten/a12s/<name>:<version>` (linux/amd64 and linux/arm64), publishes the default harness bundles if
missing, then runs GoReleaser (`.goreleaser.yaml`): binaries for darwin and linux on amd64 and arm64, built with the
release version and registry, and the formula in `coltonkirsten/homebrew-tap`. Last it publishes
the `a12s` package to npm (with provenance), if the `NPM_TOKEN` secret is set, and to PyPI, if `PYPI_TOKEN` is set.

It needs the secret `HOMEBREW_TAP_DEPLOY_KEY`: the private half of a deploy key with write access to the tap repo.
`NPM_TOKEN` (an npm automation token that can publish the `@coltonkirsten` scope) is optional: without it the SDK
job skips with a warning instead of failing the release. `PYPI_TOKEN` (a PyPI API token for the `a12s` project) is
optional the same way: without it the PyPI job skips with a warning.

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
- Run `make build test vet` and, for computerd changes, `npm run lint && npm test`.
- Update the docs in `docs/` when behavior changes: they describe the current state of the code.
