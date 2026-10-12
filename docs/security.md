# Security model

a12s assumes the agent inside a Computer may be confused or prompt-injected, and limits what it can reach and
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
- Each Computer and Service has its own isolated internal Docker network with isolated gateway mode. There is no
  path to host services, to other Computers or to the Controller's owner API. Containers publish no ports, except a
  Service's explicit `publish:`.

## Egress

Every Computer's and Service's only route out is the Controller's validating egress proxy.

- Only hosts in `egress.allow` (plus the harness bundle's sign-in hosts) are reachable, over HTTPS (CONNECT to
  port 443). Plain `http://` to the internet is refused.
- The proxy resolves names itself and rejects private, link-local and cloud-metadata addresses and DNS rebinding.
  Raw UDP, external DNS and SSH are blocked; agents use HTTPS for git.
- A denied request becomes a `diagnostic` event in the Computer's journal, so you see exactly what was blocked.
- `allow: ["*"]` permits any public host on any port, for experiments. Private and metadata addresses stay blocked.
  It's shown on every status line.
- `egress.enforcement: unenforced` exists for substrates that can't enforce egress, and is shown on every status
  line.
- Services reach other Services only through `uses:`, checked by the proxy before and independently of egress
  policy; see [Services](services.md#grants-and-reachability).

## Identity

- The Controller is a built-in certificate authority. Each Computer and Service start gets a fresh certificate with
  a typed identity (`a12s://<fleet>/<kind>/<name>/<generation>`). The Controller accepts a certificate only while it
  is the live one for a running container; it is revoked before every stop, so a copied key is refused once the
  container stops.
- The Controller talks to computerd over mutual TLS.
- The owner API listens on `127.0.0.1` with a random bearer token in `~/.a12s/<fleet>/controller.json` (0600).
  Scoped tokens carry an allowlist of actions and Computers.
- Inside the Fleet, Services and Computers call the Controller with their certificates and can do only what their
  `grants` say. Messages carry `via`, the authenticated principal, which no caller can set.

## Secrets

See [Credentials](credentials.md). In short: in memory in the Controller, on tmpfs in the container, never in an
image, config, argv or on a drive.

## Inbox bodies are untrusted

Every Message body is rendered as untrusted data inside nonce-tagged frames, after a fixed safety block that tells
the agent so. See [Inbox](inbox.md#what-the-agent-sees).

## Honest limits

- Containers share the host kernel. Tenants that are hostile to each other need a sandboxed runtime (gVisor, Kata).
- Any host on an allowlist is a possible exfiltration channel.
- With `autonomy: full` (the default), the agent approves its own tool calls inside its Computer.
- MCP config the agent writes to its home drive (and a cloned repo's `.mcp.json`) is loaded after
  `restart_harness`.
- A Service program can copy its own secret into its state drive; choose Service programs the way you choose MCP
  servers.
- A credential file is still a file: the certificate registry and per-start lifetime limit the damage of a copied
  key, but can't prevent the copy.
