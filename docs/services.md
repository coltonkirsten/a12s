# Services

A **Service** is a non-agent container the Controller runs in the same Fleet as your agent computers: a clock, a
database, an MCP server that holds a credential. It has no harness, no session and no inbox. What it may
do is written in the Fleet as grants, on the side that acts. An [external Service](#external-services) is an app
that runs outside the Fleet, which agent computers call the same way.

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
| Who is calling | `A12s-Caller: <name>` on every plain-HTTP request the proxy forwards to it ([Caller](#caller)) |
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

### Caller

On every plain-HTTP request it forwards to a Service (`http://<svc>.a12s:<port>/…`), the proxy sets
`A12s-Caller` to the name of the agent computer or Service that sent it, after removing every copy the
caller sent (in any letter case, and spelled `A12s_Caller`). A Service can trust the header to tell callers apart
without handing each one a secret: nothing inside the Fleet can set it. A tunnel (`CONNECT`, for TCP ports and
HTTPS) carries bytes the proxy doesn't read, so it has no caller header.

## External Services

An external Service is an app a12s does not run, such as a web app on your machine that drives agents through the
API and offers them tools. The Fleet names it and says where it is; agent computers that `uses:` it reach it at
`http://<svc>.a12s:<port>`, through the proxy, which dials `external.host` on that same port from the Controller and
names the caller in `A12s-Caller`.

```yaml
services:
  tools:
    external: { host: host.docker.internal }    # the Docker host, as the Controller resolves it
    manifest:
      ports: { api: { port: 7400, mcp: { path: /mcp } } }
      health: { http: { port: 7400, path: /healthz } }   # optional
agent_computers:
  lead:
    uses: [tools]    # http://tools.a12s:7400 and the a12s-tools MCP tool
```

- `external.host` is a host name or IPv4 address, without scheme or port; the inline `manifest.ports` say which
  ports agent computers call, all `http`. An `mcp` port becomes a tool, as for any Service.
- Plain HTTP only: the proxy refuses `CONNECT` to an external Service, so every request carries the caller.
- `manifest.health.http` is checked against the external host; without it the Service is `Ready` as soon as it is
  admitted. An Unhealthy external Service is reported and the proxy refuses calls to it until it passes again.
- a12s runs no container for it, so it takes only `external`, `manifest.ports` and `manifest.health.http`: `image`,
  `config`, `credentials`, `egress`, `grants`, `uses`, `resources`, `state` and the manifest's `user`,
  `credentials`, `state`, `tmpfs`, `resources` and `health.exec` are refused with a fix. It can't send to inboxes
  and isn't behind the proxy; to act on the Fleet it uses the [API](api.md) with a token.
- `a12s sleep`, `a12s wake` and `a12s shell` refuse it; `a12s service logs` shows its audit lines (health
  changes). `a12s service ls` shows `external <host>` in place of the image.
- Only the owner creates, changes or removes one: it points agents at an address outside the Fleet
  ([Security: Tokens](security.md#tokens)).
- Docker driver only, like every Service. `host.docker.internal` is the Docker host on Docker Desktop and, through
  the Controller container's host entry, on Docker Engine for Linux. The app must listen where the Controller can
  reach it (on Linux, on the Docker bridge's address or `0.0.0.0`, not only `127.0.0.1`).

## Credentials

A Service's credentials are set like an agent computer's, with `a12s creds set <name>`. Until each one has a value the
Service waits in `Provisioning` with `waiting: {credentials: [postgres], fix: "a12s creds set postgres"}` while the
rest of the Fleet runs.

Honest limit: a Service program can copy its own secret into its state drive. The only secrets at rest are in the
Controller's `credentials.json` (0600, on its state volume; see [Credentials](credentials.md#how-secrets-are-handled)).

## Commands

```text
a12s service ls | status <svc> | logs <svc> [-f]
a12s sleep <svc> / a12s wake <svc>        # --all includes Services that a12s runs
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
go mod init example.com/clock && go get github.com/coltonkirsten/a12s/services/svckit@v0.10.0
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
