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
| `GET /v1/status` | → `{fleet, computers: [ComputerStatus], services: [ServiceStatus], warnings}` |
| `POST /v1/computers/{c}/inbox` | Message → `201 {id, seq, state, position, phase, createdAt, expiresAt, replaced?, turn?}` |
| `GET /v1/computers/{c}/inbox` | `?state=pending\|all\|…&bodies=true` → `{pending, claimed, limits, usage, held, messages: [Message]}` |
| `GET /v1/computers/{c}/inbox/{id}` | → Message |
| `DELETE /v1/computers/{c}/inbox/{id}` | withdraw a pending Message → Message (`409` if it is too late) |
| `GET /v1/computers/{c}/inbox/{id}/events` | SSE: a `snapshot`, then the Message's state changes (`claimed`, `delivered` with its `turn`, …) |
| `GET /v1/computers/{c}/events` | SSE: the Computer's journal (below) |
| `POST /v1/computers/{c}/turns/{turn}/stop` | `{}` → interrupt the Turn |
| `POST /v1/computers/{c}/power` | `{"state": "asleep"\|"awake"}` |
| `POST /v1/computers/{c}/inbox/hold`, `…/resume`, `…/clear` | hold, resume (`{"session": "new"}` to start fresh) or clear an inbox |
| `GET /v1/services`, `GET /v1/services/{s}` | Service status |
| `POST /v1/tokens`, `GET /v1/tokens`, `DELETE /v1/tokens/{name}` | scoped tokens (owner only) |
| `PUT /v1/fleet` | what `a12s up` sends (owner only): the Fleet, `lock`, credentials, `presets`, and `seccomp` (each `security.seccomp` path → the profile's JSON text) → the updated `lock` and what changed |

The field-by-field shapes are in `internal/api` and the SDKs' types. A ComputerStatus carries `seccomp` (the
`security.seccomp` path) when the Computer runs with its own seccomp profile.

### Sending a Message

```http
POST /v1/computers/analyst/inbox
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

`GET /v1/computers/{c}/events` is a `text/event-stream`. Each journal event is one frame whose `id` is its `seq`:

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
- A sleeping Computer answers `503 asleep`: retry, and its journal streams again once it wakes.

The event types and their `data` are listed in [Concepts](concepts.md#event-journal).

## Errors

Every error is JSON with a stable `code`, a `message`, and usually a `fix` line that says what to do:

```json
{"code": "no_grant", "message": "worker1 may not send to reviewer", "fix": "add reviewer to computers.worker1.grants.send"}
```

| Code | Status | Means |
|---|---|---|
| `invalid`, `bad_metadata`, `bad_expires` | 400 | The request or the Fleet is wrong; the message names the field. |
| `unauthorized` | 401 | Missing or unknown token. |
| `forbidden`, `no_grant` | 403 | The token or the sender lacks that action, Computer or grant. |
| `not_found`, `unknown_request` | 404 | No such Computer, Service, Message or Turn. |
| `busy`, `conflict`, `not_idle`, `idempotency_conflict` | 409 | The Computer is busy, or the request conflicts with an earlier one. |
| `withdrawn`, `discarded`, `expired`, `replaced`, `already_claimed`, `already_delivered` | 409 | The Message already ended or was taken. |
| `gone` | 410 | It existed and is gone. |
| `body_too_large`, `exceeds_inbox` | 413 | The body, or one Message, is too big for the inbox. |
| `inbox_full` | 429 | The inbox is at its limit; retry later. |
| `internal` | 500/502 | A bug or a Docker failure; the message says which. |
| `asleep` | 503 | The Computer is asleep; wake it, or send (the inbox works while asleep). |
