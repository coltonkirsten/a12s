# HTTP API

The SDKs wrap this API; use it directly from any other language. It is JSON over HTTP, served by the Controller on
`127.0.0.1` only.

## Connecting

`a12s up` writes `~/.a12s/<fleet>/controller.json` (mode 0600):

```json
{ "port": 52011, "token": "…" }
```

The port stays the same across restarts of the Controller's container; a new Controller container reuses it while it
is free.

The base URL is `http://127.0.0.1:<port>`. Send the token on every request as `Authorization: Bearer <token>`. It
is the owner token; `POST /v1/tokens` (or `a12s token create`) makes [scoped ones](#tokens).
`a12s gateway` serves the same routes to a web page on its own origin, without a token
([SDK: In the browser](sdk.md#in-the-browser)).

## Routes

| Route | Body → response |
|---|---|
| `GET /v1/status` | → `{fleet, agentComputers: [ComputerStatus], services: [ServiceStatus], warnings, …}` |
| `GET /v1/agent-computers` | → `{agentComputers: [AgentComputer]}` |
| `GET /v1/agent-computers/{c}` | → AgentComputer: `{name, spec, effective, status}` |
| `PUT /v1/agent-computers/{c}` | its spec → create or replace it (owner or prefix; [below](#changing-the-fleet)) |
| `PATCH /v1/agent-computers/{c}` | a JSON merge patch → change some fields (owner or prefix) |
| `DELETE /v1/agent-computers/{c}` | → remove it; its drives and inbox are kept (owner or prefix) |
| `POST /v1/agent-computers/{c}/inbox` | Message → `201 {id, seq, state, position, phase, createdAt, expiresAt, replaced?, turn?}` |
| `GET /v1/agent-computers/{c}/inbox` | `?state=pending\|all\|…&bodies=true` → `{pending, claimed, limits, usage, held, messages: [Message]}` |
| `GET /v1/agent-computers/{c}/inbox/{id}` | → Message |
| `DELETE /v1/agent-computers/{c}/inbox/{id}` | withdraw a pending Message → Message (`409` if it is too late) |
| `GET /v1/agent-computers/{c}/inbox/{id}/events` | SSE: a `snapshot`, then the Message's state changes (`claimed`, `delivered` with its `turn`, …) |
| `GET /v1/events` | SSE: every agent computer's events and the Fleet's own on one connection (below); `?agentComputers=a,b` filters |
| `GET /v1/agent-computers/{c}/events` | SSE: the agent computer's journal (below) |
| `GET /v1/agent-computers/{c}/turns` | `?limit=20` (1 to 200) → `{turns: [Turn]}`: its last finished Turns, newest first |
| `POST /v1/agent-computers/{c}/turns/{turn}/stop` | `{}` → interrupt the Turn and end the session's background work |
| `POST /v1/agent-computers/{c}/stop` | `{}` → `{turns, background}`: interrupt whatever Turn runs and end the session's background work; what it stopped |
| `POST /v1/agent-computers/{c}/power` | `{"state": "asleep"\|"awake"}` |
| `POST /v1/agent-computers/{c}/inbox/hold`, `…/resume`, `…/clear` | hold, resume (`{"session": "new"}` to start fresh) or clear an inbox |
| `GET /v1/services`, `GET /v1/services/{s}` | Service status, with `spec` as written |
| `PUT`, `PATCH`, `DELETE /v1/services/{s}` | create or replace, change, remove a Service, like an agent computer (owner or prefix) |
| `POST /v1/tokens`, `GET /v1/tokens`, `DELETE /v1/tokens/{name}` | [scoped tokens](#tokens) (owner only) |
| `GET /v1/credentials` | → `{credentials: [{name, set, usedBy, updatedAt?}]}`: every credential the Fleet names or the Controller stores, never a value (owner; a prefix token sees names inside its prefix) |
| `PUT /v1/credentials/{name}` | `{"value": "…"}` → `{name, set, usedBy, updatedAt}`: store it; agent computers and Services that use it get it at once (owner or prefix) |
| `DELETE /v1/credentials/{name}` | → 204; `409 in_use` while one uses it, `404` if none is stored (owner or prefix) |
| `GET /v1/fleet` | → `{version, fleet}` with `ETag: "<version>"` (owner only) |
| `PUT /v1/fleet` | `{fleet, credentials?}` → replace the whole Fleet (owner only); `a12s up` sends this |

The field-by-field shapes are in `internal/api` and the SDKs' types. A ComputerStatus carries `seccomp` (the
profile's `sha256:` digest) when the agent computer runs with its own seccomp profile, and `background: {tasks}` while
its session runs [background work](concepts.md#background-work). A Turn has `reason: "background"` when the harness
started it by itself.

## Tokens

`POST /v1/tokens` (owner only) makes a token for another program and returns it, with its `secret`, once:

- `{"name": "bizapp", "prefix": "biz-"}`: a prefix token. On agent computers, Services and credentials whose names
  start with the prefix it may call every route the owner can, except the whole-Fleet ones (`GET`/`PUT /v1/fleet`),
  drives and tokens. What its agent computers and Services send to and use (`grants.send`, `uses`, those from
  `defaults` included) must be inside the prefix too; its specs may name any credential.
  It can't add, change or drop an agent computer's own `security.seccomp` or `attach`, or change the `image` of one
  with `attach` (`403 forbidden`): only the owner does.
  `GET /v1/status`, `/v1/agent-computers`, `/v1/services` and `/v1/credentials` list only names inside it, and its
  status and write responses carry no Fleet-wide warnings. Anything outside is `403 out_of_prefix`; the whole-Fleet
  routes, drives and tokens are `403 forbidden`. See [Security: Tokens](security.md#tokens).
- `{"name": "dash", "actions": ["status", "events"], "computers": ["a", "b"]}`: a narrow token, for those actions
  (`send`, `inbox`, `status`, `events`, `power`, `stop`) on those agent computers (or `["*"]`). A token with `send`
  sees only its own Messages.

A token has `prefix` or `actions`, not both. `GET /v1/tokens` lists them (no secrets), each with `createdAt` and
`lastUsedAt` (when it last authenticated, recorded at most once a minute; absent if never); `DELETE /v1/tokens/{name}`
revokes one. The [API surface in llms.txt](../llms.txt) lists, per route, the action a narrow token needs.

## Changing the Fleet

The Fleet is the desired state: `defaults`, `agent_computers` and `services`, as in the
[Fleet file](fleet-file.md), in JSON. Two kinds of write change it.

**One agent computer or Service.** `PUT /v1/agent-computers/{c}` takes its spec, the Fleet file's fields for one
agent computer; whatever it leaves out comes from `defaults`. `instructions` and `files` carry the text for its home,
`security.seccomp` is the profile's JSON object, and each `attach` host is an absolute path. These writes take no version: they change only the named one,
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

**The whole Fleet.** `GET /v1/fleet` returns `{version, fleet}` and the version as the `ETag`. `PUT /v1/fleet`
replaces everything:

```http
PUT /v1/fleet
Authorization: Bearer …
If-Match: "12"
Content-Type: application/json

{"fleet": {"apiVersion": "a12s.ai/v1alpha1", "kind": "Fleet", "metadata": {"name": "team"}, "defaults": {…}, "agent_computers": {…}}}
```

With `If-Match`, a Fleet that changed since you read it answers `412 version_mismatch`: read it again and retry.
Without it, the write is unconditional. `credentials` (`{name: value}`, values base64 in JSON) are merged into the
Controller's store: names it leaves out keep their values. The version goes up by one with every write that changes
something. Only the Fleet file can hold paths relative to it: a `security.seccomp` path or a relative or `~` `attach`
host in the body is refused (`a12s up` sends the profile as JSON and the folder's absolute path).

**Every write** takes `?dryRun=true`: it checks the write as the write does (images, their Service manifests and
custom-image labels included, so it fails with the same error) and lists what it would change, and changes nothing. Every
write answers:

```json
{"version": 13, "changes": [{"op": "update", "kind": "agentComputer", "name": "lead", "when": "idle", "fields": ["instructions"]}],
 "created": [], "updated": [], "staged": ["lead"], "removed": [], "services": {…}, "warnings": []}
```

`op` is `create`, `update` or `remove`; `kind` is `agentComputer` or `service`; `when` is `now`, or `idle` for an
update that waits for a busy agent computer's next idle boundary; `fields` names the top-level fields that differ.
`diff`, on an update, lists every leaf that differs with its value before and after:
`[{"path": "grants.send", "after": ["scout"]}, {"path": "instructions", "before": "a", "after": "b"}]`. `path` is the
dotted field path in the spec (objects are walked; lists and values are leaves), the values are the spec's with the
defaults merged in, and `before` or `after` is left out where the field is absent.
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

`GET /v1/events` is the whole Fleet on one connection: every agent computer's events, plus the Controller's own:

- `fleet.changed` `{version, by, names}`: the Fleet's version moved.
- `agent_computer.phase` `{phase, error?, waiting?}`: an agent computer's phase changed. `waiting`
  `{credentials, fix}` is set while it stays `Provisioning` for credentials with no value yet.
- `inbox.changed`: an agent computer's inbox changed (a Message arrived, was withdrawn, delivered or expired; a hold
  or resume; a clear). Its data is the inbox summary of `GET /v1/agent-computers/{c}/inbox` without `messages`
  (`{computer, pending, claimed, held, holdReason?, limits, usage, overLimit, ended24h, …}`). Changes within about
  100 ms are one event.
- `service.phase` `{service, phase, health?, restarts, error?, waiting?}`: a Service's phase, health or restart count
  changed.
  Its `agentComputer` is empty; it is filtered by the Service's name.

All of it is filtered to what the token may watch (a prefix token sees only its prefix) and to `?agentComputers=a,b`. A browser opens at most six connections per origin, so
one stream per agent computer would stall a page that watches more than a few.

- Frame ids are opaque strings. `Last-Event-ID` replays what came after it if the Controller still holds it (the last
  4096 events, while the same Controller runs), then goes live; otherwise the stream sends `event: gap` first.
- `event: head` (`data: {"id": …}`) comes once the replay is done: from here the stream is live.
- `event: gap` (`data: {}`) means events were lost: resync from `/v1/status` and the turn history, and carry on.

`GET /v1/agent-computers/{c}/events` is one agent computer's journal. Each event is one frame whose `id` is its `seq`:

```text
id: 42
data: {"v":1,"agentComputer":"analyst","session":"…","turn":"t_9","seq":42,"ts":"…","type":"text.delta","data":{"text":"Hel"}}
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
| `unauthorized` | 401 | Missing or unknown token. |
| `forbidden`, `no_grant` | 403 | The token or the sender lacks that action, agent computer or grant. |
| `out_of_prefix` | 403 | A prefix token named something, or wrote a spec that points at something (`grants.send`, `uses`, also from `defaults`), whose name doesn't start with its prefix. |
| `not_found`, `unknown_request` | 404 | No such agent computer, Service, Message or Turn. |
| `busy`, `conflict`, `not_idle`, `idempotency_conflict` | 409 | The agent computer is busy, or the request conflicts with an earlier one. |
| `exists` | 409 | A create-only write (`If-None-Match: *`) named an agent computer or Service that exists. |
| `in_use` | 409 | Removing a credential that an agent computer or Service still names; the message lists them. |
| `attached` | 409 | `DELETE /v1/drives/{d}` on a drive an agent computer or Service is using. |
| `withdrawn`, `discarded`, `expired`, `replaced`, `already_claimed`, `already_delivered` | 409 | The Message already ended or was taken. |
| `gone` | 410 | It existed and is gone. |
| `version_mismatch` | 412 | `If-Match` names an old Fleet version: someone wrote in between. Read and retry. |
| `body_too_large`, `exceeds_inbox` | 413 | The body, or one Message, is too big for the inbox. |
| `inbox_full` | 429 | The inbox is at its limit; retry later. |
| `internal` | 500/502 | A bug or a Docker failure; the message says which. |
| `asleep` | 503 | The agent computer is asleep; wake it, or send (the inbox works while asleep). |
