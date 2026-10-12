# Extending a12s

a12s is a pinned dependency, installed like Docker. A project never vendors, forks or edits it. Everything a project
customizes goes through four surfaces:

| Surface | What you change | Where it lives |
|---|---|---|
| The Fleet file | Computers, harness versions and models, credentials, drives, egress, grants, Services | your `fleet.yaml` |
| Images | Agent images built FROM an a12s base; any image as a Service | your Dockerfiles and registry |
| The SDK | Apps that send work, read inboxes and stream events | `@coltonkirsten/a12s` on npm, `a12s` on PyPI ([SDK](sdk.md)) |
| `svckit` | Custom Services written in Go ([Services: Write a Service with svckit](services.md#write-a-service-with-svckit)) | `github.com/coltonkirsten/a12s/services/svckit` |

a12s holds no app policy. Who talks to whom, what a task means, and how results combine belong in your app and
your Fleet's grants, not in a12s.

The examples assume a release binary (`a12s version` prints `v0.3.0`), whose images live at
`ghcr.io/coltonkirsten/a12s`. In a source build, use the local refs instead (`a12s/slim:dev`).

## A custom agent image

`image:` takes `slim` or your own image built FROM it. Use this to add tools an agent needs (a toolchain, a CLI,
fonts, a browser).

```dockerfile
# images/agent-jq/Dockerfile
FROM ghcr.io/coltonkirsten/a12s/slim:0.3.0
RUN apt-get update \
 && apt-get install -y --no-install-recommends jq \
 && rm -rf /var/lib/apt/lists/*
```

```sh
docker build -t my-org/agent-jq:1 images/agent-jq
```

```yaml
computers:
  analyst:
    harness: { kind: claude-code, version: 2.1.280 }
    image: my-org/agent-jq:1
    credentials: [anthropic]
```

`a12s up` pulls the image if it isn't local (with your `docker login`), pins its ID in `fleet.lock` under its ref,
and checks it at admission:

- it carries the `ai.a12s.computerd.version` label, which the base images set and every image built FROM them
  inherits;
- computerd is at `/opt/a12s/computerd/dist/main.js`;
- the label's release is compatible with the Controller's: the same major version, and while a12s is 0.x the same
  minor (a v0.2 Controller refuses an image built FROM v0.1). If either side is a `dev` build, `up` admits the image
  and prints a warning instead.

Otherwise nothing changes and `up` says why:

```text
computers.analyst.image: my-org/agent-jq:1 has no ai.a12s.computerd.version label, so it was not built FROM an a12s image  fix: start its Dockerfile with FROM ghcr.io/coltonkirsten/a12s/slim:0.3.0
```

What a custom image can't change: a12s always starts computerd as root (your `ENTRYPOINT`, `CMD`, `USER` and
`HEALTHCHECK` are ignored), the root filesystem is read-only, the harness runs as uid 1000, and the home drive is
mounted over `/home`. Install tools system-wide (under `/usr` or `/opt`), not into `/home/agent`. To pick up a new
image, change its tag in `fleet.yaml`: a pinned ref keeps running the pinned ID. When you upgrade a12s to a new minor
(or, from 1.0, a new major), rebuild FROM the new base; admission refuses the old image until you do.

An image can't change its own sandbox. If its software needs syscalls Docker's default seccomp profile blocks
(Chromium's sandbox does), set [`security.seccomp`](security.md#seccomp) on the Computer in `fleet.yaml`; see
[Images: A browser in your image](images.md#a-browser-in-your-image).

## A custom Service

A Service is any image with a manifest. Either bake `/a12s/service.json` into the image or describe a stock image
inline. Both are documented in [Services](services.md#manifest).

**A stock image, described inline** (Redis, with a state drive):

```yaml
services:
  cache:
    image: redis:7-alpine
    manifest:
      user: 999
      ports: { redis: { port: 6379, protocol: tcp } }
      health: { exec: ["redis-cli", "ping"] }
      state: { at: /data, size: 1Gi }
computers:
  analyst:
    uses: [cache]          # reaches cache.a12s:6379; nothing else in the Fleet can
```

**Your own image.** Bake the manifest into an image whose program reads its config and credentials from
`/run/a12s` and calls the Controller as its grants allow. In Go, `svckit` does that for you:
[Services: Write a Service with svckit](services.md#write-a-service-with-svckit) builds a clock end to end. Any other
language uses the files and variables in [What a Service gets](services.md#what-a-service-gets).

## Harness version and model

```yaml
computers:
  analyst:
    harness: { kind: claude-code, version: 2.1.280, model: claude-haiku-4-5 }
```

`version` picks the HarnessBundle `ghcr.io/coltonkirsten/a12s/bundle-<kind>:<version>`. A daily workflow publishes
every new Claude Code and Codex release there, so moving to a new harness is one edit and an `a12s up`; no image is
rebuilt. `model` is passed to the harness as is. For a version that isn't published, build the bundle from the a12s
source (`make bundle-claude-code VERSION=x.y.z`) and tag it with the registry ref above.

## Extra credentials

Name any credential in the Fleet and bind it on each machine. Names the harness knows (`anthropic`, `openai`,
`github`) become its environment; any other name is a file at `/run/a12s/secrets/<name>`.

```yaml
computers:
  analyst:
    credentials: [anthropic, github, stripe-test]
```

```yaml
# a12s.bindings.yaml (never committed)
credentials:
  anthropic:   { file: ~/.a12s/anthropic }
  github:      { env: GH_TOKEN }
  stripe-test: { file: ~/.secrets/stripe-test }
```

Prefer a Service when an agent should use a capability without holding the secret: the Service holds the
credential and the agent `uses:` it. See [Credentials](credentials.md).

## Drives

```yaml
drives:
  repo:      { size: 20Gi, access: exclusive }     # one writer
  reference: { size: 5Gi,  access: shared }        # many readers
  scratch:   { size: 5Gi,  access: shared-write }  # many writers; coordinate with the lock/release tools
computers:
  analyst:
    drives: [{ name: repo, at: /work/repo }, { name: reference, at: /work/ref, readOnly: true }]
```

Drives outlive Computers; only `a12s drive rm` and `a12s down --purge` destroy them.
[Fleet file: Drives](fleet-file.md#drives).

## Egress

Every Computer is deny-by-default. List the hosts it needs; the harness's sign-in hosts are added for you.

```yaml
defaults:
  egress: { allow: [] }
computers:
  analyst:
    egress: { allow: [github.com, api.github.com, registry.npmjs.org] }
```

Lists replace rather than merge, so a Computer's list replaces the default. `["*"]` allows any public host and shows
a warning on every status line. Services reach external TCP hosts with `egress.tcp`. See [Security](security.md).

## Apps: the SDK

Apps send work to Computers and follow what they do through the SDK (`npm install @coltonkirsten/a12s`) or the HTTP API. See
[SDK](sdk.md) and [HTTP API](api.md).
