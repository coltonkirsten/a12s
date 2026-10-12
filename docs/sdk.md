# SDK

The SDK is how a program talks to a running Fleet: send work to Computers, follow what they do, read the result.

```sh
npm install a12s
```

## Connect

```ts
import { connect } from "a12s";

const a12s = await connect();
```

`connect()` finds the running Fleet the way the CLI does: the `fleet.yaml` in the current directory or a parent
(or `$A12S_FLEET`), else the only Fleet running on this machine. It connects as the owner, using the
`~/.a12s/<fleet>/controller.json` that `a12s up` writes. Other ways in:

| You have | Use |
|---|---|
| Several Fleets running | `connect({ fleet: "myproject" })` |
| A scoped token, or a Controller elsewhere | `connect({ url, token })`, or `$A12S_URL` and `$A12S_TOKEN` |
| A web page | `a12s gateway --web <dir>` serves it; in the page, `connect()` (see [In the browser](#in-the-browser)) |

The same `import { connect } from "a12s"` works in Node, Bun, Deno and browsers; in a browser, `connect()` talks to
the page's own origin.

## Send work, get the answer

```ts
const r = await a12s.prompt("analyst", "Summarize the open issues in three bullets.");
console.log(r.status, r.text);   // "completed", what the agent said
```

`prompt()` puts a Message in the Computer's inbox, waits until a Turn delivers it, follows that Turn and resolves
when it ends. If the Computer is busy, the Message waits its turn.

`send()` only puts the Message in the inbox and returns. Use it to hand off work and move on, or when the answer
arrives some other way (for example from another Computer):

```ts
const msg = await a12s.send("analyst", "Run the nightly audit.");
const msg2 = await a12s.send("analyst", { body: "…", sender: "billing", metadata: { ticket: "T-42" }, expires: 3600 });
```

| Field | Meaning |
|---|---|
| `body` | The text, at most 64 KiB. The agent sees it as untrusted data from the sender. |
| `sender` | Your label for the sender, shown to the agent as `from`. Who really sent it is recorded as `via`. |
| `metadata` | Up to 16 string pairs, shown to the agent. |
| `fresh` | Start a new session (conversation) with this Message. |
| `expires` | Seconds to wait for delivery; `0` means now or never. |
| `replaceKey` | Replace your pending Message with the same key, if it hasn't been delivered yet. |
| `idempotencyKey` | Retries with the same key return the original Message. The SDK always sets one. |

## Follow what happens

`turns()` yields each Turn as it finishes, from one Computer, several, or all of them:

```ts
for await (const turn of a12s.turns(["lead", "worker1", "worker2"])) {
  const from = turn.messages.map((m) => m.via).join(", ");   // e.g. "computer:worker1"
  console.log(`${turn.computer} (${from}): ${turn.status}\n${turn.text}`);
}
```

A `Turn` has `computer`, `id`, `status` (`completed`, `failed` or `interrupted`), `text` (everything the agent
said), `error`, `messages` (the Messages it delivered: `id`, `sender`, `via`, …), `usage`, `startedAt` and
`finishedAt`.

`events()` is the same stream before it's folded into Turns: every event, as it happens.

```ts
for await (const ev of a12s.events()) {           // no argument: every Computer in the Fleet
  if (ev.type === "text.delta") process.stdout.write(ev.data.text);
  if (ev.type === "tool.started") console.log(`\n[${ev.computer}] ${ev.data.name}`);
}
```

Events are typed: narrow on `ev.type` and `ev.data` has the right shape. The event types are in
[Concepts](concepts.md#event-journal).

**Don't miss the start.** Streams start live. To see everything a piece of work causes, start watching first, wait
until the stream is connected, then send:

```ts
const turns = a12s.turns(["lead", "worker1", "worker2"]);
await turns.ready();                     // every stream is connected
await a12s.send("lead", "Plan the launch.");
for await (const turn of turns) { … }    // nothing that happened after ready() is missed
```

A Computer that is asleep counts as ready; its events start when it wakes. To end a stream, `break` out of the loop
or call `close()`; an `AbortSignal` (`{ signal }`) works too.

Streams reconnect by themselves after a network drop, and resume exactly where they were. If the Controller can't
resume (the Computer's journal was reset), the stream yields one `gap` event and carries on live; pass
`onGap: "throw"` to get an `A12sGapError` instead. Events were lost either way: resync with `status()`.

To replay one Turn from its start: `a12s.events("analyst", { turn: r.id })`. To resume a stream you saved:
`a12s.events("analyst", { after: lastSeq })`.

## In the browser

The Controller listens on loopback with the owner token, so a web page can't call it directly. `a12s gateway` sits
in between: it serves your page, serves this SDK at `/a12s.js`, and forwards `/v1/` to the Controller with a scoped
token the page never sees.

```sh
a12s gateway                 # the built-in dashboard: every Computer, live events, a send box
a12s gateway --web ./web     # your own app instead, at http://127.0.0.1:7012
```

```html
<script type="module">
  import { connect } from "/a12s.js";
  const a12s = await connect();                       // same origin, no token
  for await (const ev of a12s.events()) console.log(ev.computer, ev.type);
</script>
```

The page may use `status`, `events`, `send` and `inbox` on every Computer; change that with
`--allow status,events` and `--computers lead,worker1`. Messages it sends arrive `via` `token:gateway`. The gateway
answers only `127.0.0.1` and `localhost` and refuses writes from other origins, so other sites open in the same
browser can't use it. It is for this machine: to serve a page to other people, put your own server and
authentication in front of a scoped token.

With a bundler, `npm install a12s` and `import { connect } from "a12s"` work the same way.

## Everything else

| Method | What it does |
|---|---|
| `status()` | Every Computer and Service: `phase`, `session`, `activeTurn`, `inbox`, `power`, warnings. |
| `computer(name)` | One Computer's status. |
| `inbox(name, { state, bodies })` | Pending Messages (`state: "all"` includes ended ones; `bodies: true` includes the text). |
| `message(name, id)`, `withdraw(name, id)` | One Message; take back a pending one. |
| `stop(name, turn?)` | Interrupt a Turn (default: the active one). |
| `sleep(name)`, `wake(name)` | Stop a Computer and keep its disks and session; start it again. |
| `services()`, `service(name)` | Service status. |
| `createToken({ name, actions, computers })` | A scoped token for another program (owner only). |
| `putFleet(req)` | What `a12s up` sends. Most apps leave this to the CLI. |

## Errors

A refused call throws `A12sError` with the HTTP `status`, a stable `code` and usually a `fix` line, which is also in
the message:

```ts
try {
  await a12s.send("lead", "…");
} catch (err) {
  if (err instanceof A12sError && err.code === "inbox_full") { /* back off */ }
  throw err;
}
```

The codes are listed in [HTTP API](api.md#errors). Event streams also throw `A12sStreamError` for a malformed
event and, with `onGap: "throw"`, `A12sGapError`.

## Least privilege

The owner token can do anything. Give another program a scoped token instead:

```sh
a12s token create dashboard --actions status,events --computers '*'
```

```ts
const a12s = await connect({ url: "http://127.0.0.1:52011", token: process.env.A12S_TOKEN });
```

Actions: `send`, `inbox`, `status`, `events`, `power`, `stop`. A token with `send` sees only its own Messages.
