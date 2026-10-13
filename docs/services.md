# Services

A **Service** is a non-agent container the Controller runs in the same Fleet as your agent computers: a clock, a
database, an MCP server that holds a credential. It has no harness, no session and no inbox. What it may
do is written in the Fleet as grants, on the side that acts.

A Service holds a capability so agents don't have to: agent computers that `uses:` it reach it over a port and never see
its credential. A Service with `grants.send` can enqueue into inboxes, which is all a clock or a fan-out
needs.

```yaml
services:
  clock:
    image: my-org/clock:1           # the Controller pins the image ID on first use
    grants: { send: [scout] }        # may enqueue to scout's inbox; nothing else
    config: { to: scout, every: 15m }
agent_computers:
  scout:
    uses: [db]                       # may reach db's declared ports; db's MCP port becomes the a12s-db tool
```

Services are also created, changed and removed while the Fleet runs, with the same fields in JSON:
`fleet.services.create("clock", image=…, grants={"send": ["scout"]}, config={…})` in Python
(`fleet.services.create("clock", { image, grants, config })` in TypeScript), `service.update(…)`,
`service.remove()`, or `PUT`, `PATCH` and `DELETE /v1/services/{s}` ([HTTP API](api.md#changing-the-fleet)). A
Service has no idle boundary, so a change restarts it at once.

## What a Service gets

| Thing | Where |
|---|---|
| Its config (`services.<name>.config`) | `/run/a12s/config/config.json`; its `env:` map is exported into the environment |
| Its credentials (`credentials: [...]`) | `/run/a12s/secrets/<name>`, owned by the manifest uid, 0400, on tmpfs |
| Its identity toward the Controller | `A12S_API` and `/run/a12s/tls/{ca.crt,cert.pem,key.pem}`; a fresh certificate per start, refused once it stops |
| Used Services | `A12S_SERVICE_<SVC>_<PORT>`: `http://<svc>.a12s:<port>` for HTTP ports, `127.0.0.1:<port>` (a local forwarder) for TCP ports |
| The internet | only through the egress proxy (`HTTPS_PROXY`), under its own `egress.allow` |
| Its state drive | the manifest's `state.at`, a Drive named `<svc>-state` that `down` keeps; only `a12s drive rm` or `down --purge` destroys it |

The image's process runs as the manifest uid (never 0) with no capabilities, on a read-only root filesystem, with no
Docker socket and no host mounts. `a12s-init` (the `a12s` binary, mounted read-only) does the privileged part of the
start, then drops to the manifest uid and execs the image's own entrypoint and command.

## Manifest

Every Service image carries `/a12s/service.json`. A stock image can be described inline under
`services.<name>.manifest:` instead; inline fields override the image's, field by field.

```json
{
  "a12s": 1,
  "name": "db-mcp",
  "version": "0.1.0",
  "user": 1000,
  "health": { "http": { "port": 8080, "path": "/healthz" }, "interval": "10s", "startup": "60s" },
  "ports": { "mcp": { "port": 8080, "protocol": "http", "mcp": { "path": "/mcp" } } },
  "credentials": [ { "name": "postgres", "required": false } ],
  "state": { "at": "/var/lib/svc", "size": "1Gi" },
  "tmpfs": ["/var/cache/svc"],
  "resources": { "cpu": 0.5, "memory": "512Mi" }
}
```

- `user`: a uid, or `"uid:gid"`; 0 is refused.
- `health`: `http {port, path}` (a 2xx or 3xx passes)
  or `exec [command...]` (exit 0 passes). Ready = a passing check; Unhealthy is reported, never restarted. A Service
  that exits is restarted by the Controller with backoff.
- `ports`: the only ports anyone may reach, at `<svc>.a12s:<port>` through the proxy. `protocol` is `http` or `tcp`.
  An `mcp` port is registered with the harness of every agent computer that uses the Service.
- `state` and `tmpfs`: every `VOLUME` the image declares must be the state path, or a tmpfs path or under one.

## Grants and reachability

- `grants.send: [agent computers]` (on a Service or an agent computer): may enqueue to those inboxes and withdraw
  its own Messages, over the mTLS listener (`POST /v1/agent-computers/{c}/inbox`). Anything else is `403 no_grant`
  with the fix, and appears in `a12s service logs <svc>` (or the agent computer's journal).
- `uses: [services]` (on an agent computer or a Service): may open connections to those Services' declared ports. The
  proxy decides `.a12s` names before, and separately from, egress policy; `allow: ["*"]` grants nothing here.
  `uses: [{name: db, mcp: false}]` keeps reachability and skips the tool.

## Credentials

A Service's credentials are set like an agent computer's, with `a12s creds set <name>`. Until each one has a value the
Service waits in `Provisioning` with `waiting: {credentials: [postgres], fix: "a12s creds set postgres"}` while the
rest of the Fleet runs.

Honest limit: a Service program can copy its own secret into its state drive. The only secrets at rest are in the
Controller's `credentials.json` (0600, on its state volume; see [Credentials](credentials.md#how-secrets-are-handled)).

## Commands

```text
a12s service ls | status <svc> | logs <svc> [-f]
a12s sleep <svc> / a12s wake <svc>        # --all includes Services
a12s shell <svc>                          # audited
```

`a12s service status <svc>` shows the image ID the Service's ref is [pinned](fleet-file.md#pins) to; to roll out a
rebuilt image, give it a new tag.

## Write a Service with svckit

`svckit` (`github.com/coltonkirsten/a12s/services/svckit`) is a small Go library, standard library only, for the
files and variables above: `LoadConfig` reads the config, `Secret` a credential, `NewClient` builds the mTLS client
whose `Send` and `Get` enqueue and read back Messages as far as the grants allow, `Health` registers `/healthz`, and
`ParseDuration` reads `0`, `90s`, `15m`, `1h` or `7d`. A clock that sends on an interval:

```sh
go mod init example.com/clock && go get github.com/coltonkirsten/a12s/services/svckit@v0.7.1
```

```go
// main.go
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
		if _, _, err := c.Send(cfg.To, svckit.Enqueue{Body: "Status check: anything blocked?", Sender: "clock"}); err != nil {
			log.Print(err) // a 403 no_grant names the missing grant
		}
	}
}
```

```json
{ "a12s": 1, "name": "clock", "version": "1.0.0", "user": 1000,
  "health": { "http": { "port": 8080, "path": "/healthz" } } }
```

```dockerfile
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/clock .

FROM alpine:3
COPY --from=build /out/clock /usr/local/bin/clock
COPY service.json /a12s/service.json
USER 1000:1000
CMD ["/usr/local/bin/clock"]
```

`docker build -t my-org/clock:1 .`, then use it as in the example at the top of this page. A Service in another
language does the same with the files and variables in [What a Service gets](#what-a-service-gets): it sends with
`POST $A12S_API/v1/agent-computers/{c}/inbox` (the body of [Sending a Message](api.md#sending-a-message)) and reads
back with `GET …/inbox/{id}`, presenting the certificate in `/run/a12s/tls/`.

A complete, runnable Go Service (this clock, with its Fleet and a program that watches the agent's Turns) is in the
cookbook: [examples/go-service](https://github.com/coltonkirsten/a12s-cookbook/tree/main/examples/go-service).
