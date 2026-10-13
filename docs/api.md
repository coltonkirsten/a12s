# HTTP API

The SDKs wrap this API; use it directly from any other language. It is JSON over HTTP, served by the Controller on
`127.0.0.1` only.

## Connecting

`a12s up` writes `~/.a12s/<fleet>/controller.json` (mode 0600):

```json
{ "port": 52011, "token": "…" }
```

The base URL is `http://127.0.0.1:<port>`. Send the token on every request as `Authorization: Bearer <token>`. It
is the owner token; `a12s token create` makes scoped ones (see [Inbox: scoped tokens](inbox.md#scoped-tokens)).
`a12s gateway` serves the same routes to a web page on its own origin, without a token
([SDK: In the browser](sdk.md#in-the-browser)).

## Routes

| Route | Body → response |
|---|---|
| `GET /v1/status` | → `{fleet, agentComputers: [ComputerStatus], services: [ServiceStatus], warnings, …}` |
| `GET /v1/agent-computers` | → `{agentComputers: [AgentComputer]}` |
| `GET /v1/agent-computers/{c}` | → AgentComputer: `{name, spec, effective, status}` |
| `PUT /v1/agent-computers/{c}` | its spec → create or replace it (owner only; [below](#changing-the-fleet)) |
| `PATCH /v1/agent-computers/{c}` | a JSON merge patch → change some fields (owner only) |
| `DELETE /v1/agent-computers/{c}` | → remove it; its drives and inbox are kept (owner only) |
| `POST /v1/agent-computers/{c}/inbox` | Message → `201 {id, seq, state, position, phase, createdAt, expiresAt, replaced?, turn?}` |
| `GET /v1/agent-computers/{c}/inbox` | `?state=pending\|all\|…&bodies=true` → `{pending, claimed, limits, usage, held, messages: [Message]}` |
| `GET /v1/agent-computers/{c}/inbox/{id}` | → Message |
| `DELETE /v1/agent-computers/{c}/inbox/{id}` | withdraw a pending Message → Message (`409` if it is too late) |
| `GET /v1/agent-computers/{c}/inbox/{id}/events` | SSE: a `snapshot`, then the Message's state changes (`claimed`, `delivered` with its `turn`, …) |
| `GET /v1/agent-computers/{c}/events` | SSE: the agent computer's journal (below) |
| `POST /v1/agent-computers/{c}/turns/{turn}/stop` | `{}` → interrupt the Turn |
| `POST /v1/agent-computers/{c}/power` | `{"state": "asleep"\|"awake"}` |
| `POST /v1/agent-computers/{c}/inbox/hold`, `…/resume`, `…/clear` | hold, resume (`{"session": "new"}` to start fresh) or clear an inbox |
| `GET /v1/services`, `GET /v1/services/{s}` | Service status, with `spec` as written |
| `PUT`, `PATCH`, `DELETE /v1/services/{s}` | create or replace, change, remove a Service, like an agent computer (owner only) |
| `POST /v1/tokens`, `GET /v1/tokens`, `DELETE /v1/tokens/{name}` | scoped tokens (owner only) |
| `GET /v1/fleet` | → `{version, fleet, drift}` with `ETag: "<version>"` (owner only) |
| `PUT /v1/fleet` | `{fleet, lock?, credentials?, asleep?, refresh?}` → replace the whole Fleet (owner only); `a12s up` sends this |

The paths of earlier releases, `/v1/computers/...`, are still served as `/v1/agent-computers/...` for one release.
`GET /v1/status` also still carries `computers`, the same list as `agentComputers`, for one release.

The field-by-field shapes are in `internal/api` and the SDKs' types. A ComputerStatus carries `seccomp` (the
profile's `sha256:` digest) when the agent computer runs with its own seccomp profile.

## Changing the Fleet

The Fleet is the desired state: `defaults`, `agent_computers`, `services` and `drives`, as in the
[Fleet file](fleet-file.md), in JSON. Two kinds of write change it.

**One agent computer or Service.** `PUT /v1/agent-computers/{c}` takes its spec, the Fleet file's fields for one
agent computer; whatever it leaves out comes from `defaults`. `instructions` and `files` carry the text for its home,
and `security.seccomp` is the profile's JSON object. These writes take no version: they change only the named one,
and the last write wins.

```http
PUT /v1/agent-computers/coder
Authorization: Bearer …
If-None-Match: *
Content-Type: application/json

{"instructions": "You write and run Python.", "egress": {"allow": ["pypi.org"]}}
```

- `If-None-Match: *` makes the PUT create-only: a name that is taken answers `409 exists`. Without it, a PUT replaces
  the spec.
- `PATCH` takes a [JSON merge patch](https://www.rfc-editor.org/rfc/rfc7386): objects merge, anything else replaces,
  and `null` removes a field, so the default applies again.
- A PUT or PATCH answers with the written one in `agentComputer` (`service` for a Service), after it is applied.
- The same three verbs work on `/v1/services/{s}`, with the Service fields.

**The whole Fleet.** `GET /v1/fleet` returns `{version, fleet, drift}` and the version as the `ETag`. `drift` lists
what changed through the API since the last `a12s up`, which undoes it. `PUT /v1/fleet` replaces everything:

```http
PUT /v1/fleet
Authorization: Bearer …
If-Match: "12"
Content-Type: application/json

{"fleet": {"apiVersion": "a12s.ai/v1alpha1", "kind": "Fleet", "metadata": {"name": "team"}, "defaults": {…}, "agent_computers": {…}}}
```

With `If-Match`, a Fleet that changed since you read it answers `412 version_mismatch`: read it again and retry.
Without it, the write is unconditional. The version goes up by one with every write that changes something. Only
the Fleet file can hold paths: a `home.preset` or a `security.seccomp` path in the body is refused (`a12s up` sends
the preset as `files` and the profile as JSON).

**Every write** takes `?dryRun=true`: it checks the write and lists what it would change, and changes nothing. Every
write answers:

```json
{"version": 13, "changes": [{"op": "update", "kind": "agentComputer", "name": "lead", "when": "idle", "fields": ["instructions"]}],
 "created": [], "updated": [], "staged": ["lead"], "removed": [], "services": {…}, "warnings": []}
```

`op` is `create`, `update` or `remove`; `kind` is `agentComputer` or `service`; `when` is `now`, or `idle` for an
update that waits for a busy agent computer's next idle boundary; `fields` names the top-level fields that differ.
`errors` maps an agent computer or Service that did not converge to why; the rest was still applied.

### Sending a Message

```http
POST /v1/agent-computers/analyst/inbox
Authorization: Bearer …
Content-Type: application/json

{"body": "Summarize the open issues.", "sender": "app", "idempotencyKey": "6f1c…"}
```

| Field | Type | Meaning |
|---|---|---|
| `body` | string, required | At most 64 KiB of UTF-8. |
| `sender` | string | Your label for the sender (`from` in the agent's view), up to 64 chars of `[A-Za-z0-9._:@/#-]`. |
| `metadata` | object of strings | Up to 16 pairs. |
| `fresh` | bool | Start a new session with this Message. |
| `idempotencyKey` | string | A retry with the same key returns the original Message. Always send one. |
| `expires` | number, seconds | `0`: deliver now or refuse (`409 not_idle`); otherwise expire if not delivered in time. |
| `replaceKey` | string | Supersede your pending Message with the same key. |

The response's `state` is `pending` until a Turn takes it, then `claimed` and `delivered` (with `turn`), or one of
`withdrawn`, `discarded`, `expired`, `replaced`.

## Event streams (SSE)

`GET /v1/agent-computers/{c}/events` is a `text/event-stream`. Each journal event is one frame whose `id` is its `seq`:

```text
id: 42
data: {"v":1,"computer":"analyst","session":"…","turn":"t_9","seq":42,"ts":"…","type":"text.delta","data":{"text":"Hel"}}
```

- **Where it starts.** With no cursor, the stream starts live. With `Last-Event-ID: <seq>` it replays everything
  after that seq, then goes live. With `?turn=<id>` it replays that Turn from its start, then follows it.
- **`event: head`** comes once, when the replay is done: `data: {"seq": N}`. Everything up to N has been sent; from
  here the stream is live. Use N as your cursor even if no event arrived.
- **`event: gap`** means the cursor isn't in the journal (it was reset): `data: {"lastEventId", "head"}`. Events
  were lost; resync from `/v1/status` and carry on live.
- Comment lines (`: keepalive`) arrive every 15 s.
- A sleeping agent computer answers `503 asleep`: retry, and its journal streams again once it wakes.

The event types and their `data` are listed in [Concepts](concepts.md#event-journal).

## Errors

Every error is JSON with a stable `code`, a `message`, and usually a `fix` line that says what to do:

```json
{"code": "no_grant", "message": "worker1 may not send to reviewer", "fix": "add reviewer to agent_computers.worker1.grants.send"}
```

| Code | Status | Means |
|---|---|---|
| `invalid`, `bad_metadata`, `bad_expires` | 400 | The request or the Fleet is wrong; the message names the field. |
| `missing_field` | 400 | A required field (harness, image) is set neither on the agent computer nor in `defaults`. |
| `missing_credential` | 400 | The agent computer names a credential the Controller holds no value for; `a12s creds set` it, name it in the Fleet file and run `a12s up`. |
| `unauthorized` | 401 | Missing or unknown token. |
| `forbidden`, `no_grant` | 403 | The token or the sender lacks that action, agent computer or grant. |
| `not_found`, `unknown_request` | 404 | No such agent computer, Service, Message or Turn. |
| `busy`, `conflict`, `not_idle`, `idempotency_conflict` | 409 | The agent computer is busy, or the request conflicts with an earlier one. |
| `exists` | 409 | A create-only write (`If-None-Match: *`) named an agent computer or Service that exists. |
| `withdrawn`, `discarded`, `expired`, `replaced`, `already_claimed`, `already_delivered` | 409 | The Message already ended or was taken. |
| `gone` | 410 | It existed and is gone. |
| `version_mismatch` | 412 | `If-Match` names an old Fleet version: someone wrote in between. Read and retry. |
| `body_too_large`, `exceeds_inbox` | 413 | The body, or one Message, is too big for the inbox. |
| `inbox_full` | 429 | The inbox is at its limit; retry later. |
| `internal` | 500/502 | A bug or a Docker failure; the message says which. |
| `asleep` | 503 | The agent computer is asleep; wake it, or send (the inbox works while asleep). |
