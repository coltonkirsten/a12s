# Security model

a12s assumes the agent inside an agent computer may be confused or prompt-injected, and limits what it can reach and
change.

## Isolation

- The agent runs as uid 1000 on a read-only root filesystem with no privilege escalation, no added capabilities, no
  Docker socket and no host mounts. Only the home drive, work drives and tmpfs are writable.
- computerd runs as root with a minimal capability set (`CHOWN, DAC_OVERRIDE, FOWNER, SETUID, SETGID, KILL`) and
  `no-new-privileges`. Nothing else in the container runs as root.
- The journal and session record live in `/home/.a12s`, which the agent can neither open nor rename, so an agent
  can't read, edit or erase its own record.
- The harness comes from a read-only bundle. A prompt injection can't patch the harness and keep the patch across a
  sleep.
- Each agent computer and Service has its own isolated internal Docker network with isolated gateway mode. There is
  no path to host services, to other agent computers or to the Controller's owner API. Containers publish no ports, except a
  Service's explicit `publish:`.
- Agent computers and Services run under Docker's default seccomp profile, unless the Fleet sets
  `security.seccomp` on an agent computer (below).

## Seccomp

Some software in a custom image needs syscalls Docker's default profile blocks; Chromium's own sandbox, for example,
needs user namespaces (`clone`/`unshare` with `CLONE_NEWUSER`). `security.seccomp` gives one agent computer (or
every agent computer, from `defaults`) its own profile instead of turning that sandbox off:

```yaml
agent_computers:
  browser:
    image: ghcr.io/you/browser-agent:1
    security:
      seccomp: ./profiles/chromium.json     # relative to fleet.yaml
```

- **It replaces only the seccomp filter.** The agent computer keeps `no-new-privileges`, the same capabilities, the
  read-only root filesystem and everything else above. There is no privileged mode and no capability option.
- **Only the Fleet's owner, or a prefix token for its own agent computers, sets it.** It comes from `fleet.yaml` or
  from a write through the SDK or the HTTP API, never from an image label or file, so a pulled image can't loosen its
  own sandbox.
- **It travels inline.** In the Fleet file it is a path relative to the file (or the profile itself, as a mapping);
  `a12s up` reads the file and sends the profile's JSON. Through the SDK and the HTTP API, `security.seccomp` is the
  profile's JSON object; a path there is refused.
- **It is checked before anything changes.** `a12s up` and the Controller refuse a profile unless it is a seccomp
  profile in Docker's JSON format with only the known top-level fields (`defaultAction`, `defaultErrnoRet`, `architectures`,
  `archMap`, `syscalls`, `flags`), every syscall rule naming its syscalls and a valid `SCMP_ACT_*` action. Profiles
  that hand decisions to an outside process (`SCMP_ACT_NOTIFY`, `listenerPath`) are refused, and so is one that lists
  no syscalls with a default other than `SCMP_ACT_ALLOW`. Errors name the field:
  `agent_computers.browser.security.seccomp: ./profiles/chromium.json: …`.
- **It is visible.** The profile's sha256 is part of the agent computer's generation: changing the profile restages
  it at its next idle boundary, like any other change. `a12s up` prints `<name> runs with its own seccomp profile`,
  `a12s status` shows `seccomp sha256:…` on that agent computer's line, and its status in the API carries the digest
  as `seccomp`.
- A profile whose `defaultAction` is `SCMP_ACT_ALLOW` is accepted with a warning: it allows every syscall it does
  not list.

Start from [Docker's default profile](https://github.com/moby/profiles/blob/main/seccomp/default.json) and add only
what the software needs; the cookbook's
[browser-agent](https://github.com/coltonkirsten/a12s-cookbook/tree/main/examples/browser-agent) profile does that
for Chromium.

## Egress

Every agent computer's and Service's only route out is the Controller's validating egress proxy.

- Only hosts in `egress.allow` (plus the harness bundle's sign-in hosts) are reachable, over HTTPS (CONNECT to
  port 443). Plain `http://` to the internet is refused.
- The proxy resolves names itself and rejects private, link-local and cloud-metadata addresses and DNS rebinding.
  Raw UDP, external DNS and SSH are blocked; agents use HTTPS for git.
- A denied request becomes a `diagnostic` event in the agent computer's journal, so you see exactly what was blocked.
- `allow: ["*"]` permits any public host on any port, for experiments. Private and metadata addresses stay blocked.
  It's shown on every status line.
- `egress.enforcement: unenforced` exists for substrates that can't enforce egress, and is shown on every status
  line.
- Services reach other Services only through `uses:`, checked by the proxy before and independently of egress
  policy; see [Services](services.md#grants-and-reachability).

## Identity

- The Controller is a built-in certificate authority. Each agent computer and Service start gets a fresh certificate with
  a typed identity (`a12s://<fleet>/<kind>/<name>/<generation>`). The Controller accepts a certificate only while it
  is the live one for a running container; it is revoked before every stop, so a copied key is refused once the
  container stops.
- The Controller talks to computerd over mutual TLS.
- The owner API listens on `127.0.0.1` with a random bearer token in `~/.a12s/<fleet>/controller.json` (0600).
- `a12s gateway` listens on `127.0.0.1`, answers only loopback host names, and needs a key made for each run (a
  cookie after the printed link is opened, or a bearer token) before it forwards `/v1/` with its scoped token.
- Other programs get one of the [tokens](#tokens) below instead of the owner token.
- Inside the Fleet, Services and agent computers call the Controller with their certificates and can do only what their
  `grants` say. Messages carry `via`, the authenticated principal, which no caller can set.

## Tokens

Three kinds of bearer token call the owner API:

- **The owner token** (the CLI, the console, `~/.a12s/<fleet>/controller.json`) may do everything.
- **A prefix token** (`a12s token create bizapp --prefix biz-`) gives an app its own lane. For agent computers,
  Services and credentials whose names start with `biz-` it may do everything the owner can: create, read, change,
  remove, send, read inboxes and events, sleep, wake, stop, shell, logs and Service login. Outside the prefix it
  sees and does nothing:
  - Reads are filtered: status, the agent computer and Service lists and the credential list show only names inside
    the prefix (status drops Fleet-wide warnings and orphans). Naming anything outside is `403 out_of_prefix`.
  - What its agent computers and Services point at must be inside the prefix too: `grants.send`, `uses` and
    `drives`, including what they inherit from `defaults`. A default that points outside makes the write fail with
    `out_of_prefix`; the agent computer's own value (`grants: {send: []}`) overrides it.
  - It can't read or change the whole Fleet (`GET`/`PUT /v1/fleet`, `/v1/apply`), so not `defaults`, drives or
    top-level egress, and it can't remove drives or create, list or remove tokens.
  - Its specs may name any credential, so its agent computers can use the owner's keys without seeing them. It may
    set and remove only credentials whose names start with its prefix.
- **A narrow token** (`a12s token create dash --actions status,events --agent-computers a,b`) may make only those
  actions (`send`, `inbox`, `status`, `events`, `power`, `stop`) on those agent computers (or `*`). It can't change the
  Fleet. Use it for read-only dashboards and webhooks; `a12s gateway` uses one.

There is no ceiling inside a prefix token's lane: egress (`allow: ["*"]` included), image, seccomp profile and
resources are its call, and its agent computers can use any stored credential. One rule, the prefix, is easier to get
right than a dozen limits, and it leaves an app free to shape its own agent computers. Treat a prefix token like the
owner token for the agent computers it creates, and give it only to programs you trust that far. What it can't do is
reach, read or change anything outside its prefix.

## Secrets

See [Credentials](credentials.md). In short: in the Controller's state volume (`credentials.json`, 0600), on tmpfs
in the container, never in an image, config, argv or on a drive. Anyone with access to Docker can read the stored file, but they can already exec into an agent computer and
read `/run/a12s/secrets`. No API call returns a value: `GET /v1/credentials` lists names only.

## Inbox bodies are untrusted

Every Message body is rendered as untrusted data inside nonce-tagged frames, after a fixed safety block that tells
the agent so. See [Inbox](inbox.md#what-the-agent-sees).

## Honest limits

- Containers share the host kernel. Tenants that are hostile to each other need a sandboxed runtime (gVisor, Kata).
- Any host on an allowlist is a possible exfiltration channel.
- With `autonomy: full` (the default), the agent approves its own tool calls inside its agent computer.
- MCP config the agent writes to its home drive (and a cloned repo's `.mcp.json`) is loaded after
  `restart_harness`.
- A Service program can copy its own secret into its state drive; choose Service programs the way you choose MCP
  servers.
- A credential file is still a file: the certificate registry and per-start lifetime limit the damage of a copied
  key, but can't prevent the copy.
