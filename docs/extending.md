# Extending a12s

a12s is a pinned dependency, installed like Docker. A project never vendors, forks or edits it. Everything a project
customizes goes through four surfaces:

| Surface | What you change | Where it lives |
|---|---|---|
| The Fleet file | Computers, harness versions and models, credentials, drives, egress, grants, Services | your `fleet.yaml` |
| Images | Agent images built FROM an a12s base; any image as a Service | your Dockerfiles and registry |
| The client SDK | Apps that send work, read inboxes and stream events | `@coltonkirsten/a12s-client` |
| `svckit` | Custom Services written in Go | `github.com/coltonkirsten/a12s/services/svckit` |

a12s holds no app policy. Who talks to whom, what a task means, and how results combine belong in your app and
your Fleet's grants, not in a12s.

The examples assume a release binary (`a12s version` prints `v0.1.0`), whose images live at
`ghcr.io/coltonkirsten/a12s`. In a source build, use the local refs instead (`a12s/slim:dev`).

## A custom agent image

`image:` takes `slim`, `browser`, or your own image built FROM one of them. Use this to add tools an agent needs
(a toolchain, a CLI, fonts).

```dockerfile
# images/agent-jq/Dockerfile
FROM ghcr.io/coltonkirsten/a12s/slim:0.1.0
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
- computerd is at `/opt/a12s/computerd/dist/main.js`.

Otherwise nothing changes and `up` says why:

```text
computers.analyst.image: my-org/agent-jq:1 has no ai.a12s.computerd.version label, so it was not built FROM an a12s image  fix: start its Dockerfile with FROM ghcr.io/coltonkirsten/a12s/slim:0.1.0 (or the browser tier)
```

What a custom image can't change: a12s always starts computerd as root (your `ENTRYPOINT`, `CMD`, `USER` and
`HEALTHCHECK` are ignored), the root filesystem is read-only, the harness runs as uid 1000, and the home drive is
mounted over `/home`. Install tools system-wide (under `/usr` or `/opt`), not into `/home/agent`. To pick up a new
image, change its tag in `fleet.yaml`: a pinned ref keeps running the pinned ID. When you upgrade a12s, rebuild FROM
the new base.

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

**Your own image, in Go, with svckit.** svckit is the small library the reference Services use: config, credentials,
the Service's mTLS identity, and the inbox calls its grants allow. It imports only the standard library.

```sh
export GOPRIVATE=github.com/coltonkirsten/*      # the module is private; git must be able to read it
go get github.com/coltonkirsten/a12s/services/svckit@v0.1.0
```

```go
package main

import (
	"log"
	"net/http"
	"time"

	"github.com/coltonkirsten/a12s/services/svckit"
)

type config struct {
	To    string `json:"to"`
	Every string `json:"every"`
}

func main() {
	var cfg config
	if err := svckit.LoadConfig(&cfg); err != nil {
		log.Fatal(err)
	}
	every, err := svckit.ParseDuration(cfg.Every)
	if err != nil {
		log.Fatal(err)
	}
	c, err := svckit.NewClient()
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	svckit.Health(mux)
	go http.ListenAndServe(":8080", mux)
	for range time.Tick(every) {
		if _, _, err := c.Send(cfg.To, svckit.Enqueue{Body: "Status check: anything blocked?", Sender: "heartbeat"}); err != nil {
			log.Print(err) // a 403 no_grant names the missing grant
		}
	}
}
```

```json
{ "a12s": 1, "name": "heartbeat", "version": "1.0.0", "user": 1000,
  "health": { "http": { "port": 8080, "path": "/healthz" } } }
```

```yaml
services:
  heartbeat:
    image: my-org/heartbeat:1
    grants: { send: [analyst] }
    config: { to: analyst, every: 30m }
```

A Service written in another language does the same with the files and variables in
[What a Service gets](services.md#what-a-service-gets).

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

## Apps: the client SDK

`@coltonkirsten/a12s-client` is a typed TypeScript client for the Controller's API: apply the Fleet, status, send and
prompt, inbox list/get/withdraw, the event stream and Services. It's published to npm.

```sh
npm i @coltonkirsten/a12s-client
```

```ts
import { fromFleet } from "@coltonkirsten/a12s-client/node";

const a12s = await fromFleet("myproject");   // reads ~/.a12s/myproject/controller.json
await a12s.send("analyst", { body: "Summarize the open issues.", sender: "app" });
for await (const ev of a12s.events("analyst")) {
  if (ev.type === "text.delta") process.stdout.write(ev.data.text);
  if (ev.type === "turn.finished") break;
}
```

| Method | API |
|---|---|
| `fromFleet(name)` (from `/node`) / `new A12sClient({url, token})` | owner token from `~/.a12s/<fleet>/controller.json`, or a scoped token |
| `putFleet(req)` | `PUT /v1/fleet` (what `a12s up` sends) |
| `status()`, `computer(name)` | `GET /v1/status` |
| `send(name, msg)` / `enqueue` | `POST /v1/computers/{c}/inbox`; always sends an idempotency key |
| `prompt(name, body, {onEvent})` | enqueue, wait for delivery, stream the Turn; resolves with its text and status |
| `inbox(name, {state, bodies, after})`, `message(name, id)`, `withdraw(name, id)` | the inbox endpoints |
| `events(name, {turn, after, signal, onGap})` | the journal as SSE, reconnecting with `Last-Event-ID` |
| `messageEvents(name, id)`, `waitForTurn(name, id)` | one Message's state changes; the Turn that delivered it |
| `services()`, `service(name)` | `GET /v1/services` |
| `createToken(req)`, `stop(name, turn)` | scoped tokens; interrupt a Turn |

Errors are `A12sError` with `status`, `code` and the Controller's `fix` line.

**Entry points.** `@coltonkirsten/a12s-client` has no `node:` imports and bundles for browsers, Deno and Bun (CI
checks it with esbuild `--platform=browser`). `@coltonkirsten/a12s-client/node` re-exports it and adds `fromFleet`,
which reads `~/.a12s`. A browser can't use the owner token or reach the Controller's loopback port directly: put a
small host-side gateway in front that holds the token and proxies the calls your page needs; a12s doesn't include
one.

**Event streams.** `events()` reconnects after a network drop with `Last-Event-ID`, so nothing is missed or repeated.
If the Controller no longer has the cursor (the Computer's journal was reset), it yields one event of type `"gap"`
(`data.after` is the lost cursor) and carries on live; pass `onGap: "throw"` to get an `A12sGapError` instead.
Events were lost either way: resync from `status()` and `inbox()`. `prompt()` always throws on a gap rather than
return partial text. A malformed event throws `A12sStreamError`, and an API error other than 502/503 throws
`A12sError`; neither is retried.

Give an app the least it needs with a scoped token instead of the owner token:
`a12s token create app --actions send,events --computers analyst`, then `new A12sClient({ url, token })`.
