# SDK

The SDK is how a program talks to a running Fleet: send work to Computers, follow what they do, read the result.
There is one for TypeScript (`@coltonkirsten/a12s` on npm) and one for Python (`a12s` on PyPI), with the same methods.

```sh
npm install @coltonkirsten/a12s   # Node 20+, Bun, Deno, browsers
pip install a12s                # Python 3.10+ (uv add a12s)
```

## Connect

```ts
import { connect } from "@coltonkirsten/a12s";
const a12s = await connect();
```

```python
import a12s
client = a12s.connect()
```

`connect()` finds the running Fleet the way the CLI does: the `fleet.yaml` in the current directory or a parent
(or `$A12S_FLEET`), else the only Fleet running on this machine. It connects as the owner, with the
`~/.a12s/<fleet>/controller.json` that `a12s up` writes. Other ways in:

| You have | TypeScript | Python |
|---|---|---|
| Several Fleets running | `connect({ fleet: "myproject" })` | `a12s.connect(fleet="myproject")` |
| A scoped token or another URL | `connect({ url, token })` | `a12s.connect(url=…, token=…)` |
| Environment | `$A12S_URL` and `$A12S_TOKEN` | the same |
| A web page | `a12s gateway --web <dir>`, then `connect()` in the page ([In the browser](#in-the-browser)) | |

In Python, `a12s.connect_async()` returns an `AsyncClient` with the same methods: `await` them, and use
`async for` and `async with` for streams.

## Send work, get the answer

```ts
const r = await a12s.prompt("analyst", "Summarize the open issues in three bullets.");
console.log(r.status, r.text);
```

```python
r = client.prompt("analyst", "Summarize the open issues in three bullets.")
print(r.status, r.text)
```

`prompt()` puts a Message in the Computer's inbox, waits until a Turn delivers it, follows that Turn and returns
when it ends. If the Computer is busy, the Message waits its turn. The result is a [Turn](#turns) plus `message`.

`send()` only puts the Message in the inbox and returns. Use it to hand off work and move on, or when the answer
arrives some other way (from another Computer, say):

```ts
await a12s.send("analyst", "Run the nightly audit.");
await a12s.send("analyst", { body: "…", sender: "billing", metadata: { ticket: "T-42" }, expires: 3600 });
```

```python
client.send("analyst", "Run the nightly audit.")
client.send("analyst", "…", sender="billing", metadata={"ticket": "T-42"}, expires=3600)
```

| Option (TS / Python) | Meaning |
|---|---|
| `sender` | Your label for the sender, shown to the agent as `from`. Who really sent it is recorded as `via`. |
| `metadata` | Up to 16 string pairs, shown to the agent. |
| `fresh` | Start a new session (conversation) with this Message. |
| `expires` | Seconds to wait for delivery; `0` means now or never. |
| `replaceKey` / `replace_key` | Replace your pending Message with the same key, if it hasn't been delivered yet. |
| `idempotencyKey` / `idempotency_key` | Retries with the same key return the original Message. Set for you if you don't. |

Bodies are at most 64 KiB. The agent treats them as data from the sender, not as instructions that override its
role: put instructions in its preset's `CLAUDE.md`.

## Follow what happens

### Turns

`turns()` yields each Turn as it finishes, from one Computer, several, or (no argument) all of them:

```ts
for await (const t of a12s.turns(["lead", "worker1"])) {
  console.log(t.computer, t.status, t.messages.map((m) => m.via), t.text);
}
```

```python
for t in client.turns(["lead", "worker1"]):
    print(t.computer, t.status, [m.via for m in t.messages], t.text)
```

A Turn has `computer`, `id`, `status` (`completed`, `failed` or `interrupted`), `text` (everything the agent
said), `error`, `messages` (the Messages it delivered, each with `id`, `sender` and `via`), `tools` (each tool call
with `name`, `input`, `output`, `isError` / `is_error`), `usage`, `startedAt` / `started_at` and `finishedAt` /
`finished_at`. `error` is a string; `input`, `output` and `usage` are the harness's JSON as is (a dict in Python).
What an agent sends to another Computer is in `tools`: a call named `mcp__a12s__send` whose `input`
has `computer` and `body`.

### Events

`events()` is the same stream before it's folded into Turns: every event as it happens.

```ts
for await (const ev of a12s.events()) {
  if (ev.type === "text.delta") process.stdout.write(ev.data.text);
  if (ev.type === "tool.started") console.log(`\n[${ev.computer}] ${ev.data.name}`);
}
```

```python
for ev in client.events():
    if ev.type == "text.delta":
        print(ev.data["text"], end="", flush=True)
    if ev.type == "tool.started":
        print(f"\n[{ev.computer}] {ev.data['name']}")
```

In TypeScript, events are a typed union: narrow on `ev.type` and `ev.data` has the right shape. In Python,
`ev.data` is the event's JSON as a dict. The types and their data are in [Concepts](concepts.md#event-journal).

### Don't miss the start

Streams start live. To see everything a piece of work causes, start watching, wait until the stream is connected,
then send:

```ts
const turns = a12s.turns(["lead", "worker1", "worker2"]);
await turns.ready();
await a12s.send("lead", "Plan the launch.");
for await (const t of turns) { /* … */ }
```

```python
with client.turns(["lead", "worker1", "worker2"]) as turns:   # entering waits until it's connected
    client.send("lead", "Plan the launch.")
    for t in turns:
        ...
```

A Computer that is asleep counts as connected; its events start when it wakes. To end a stream, `break`, or call
`close()` (Python: leaving the `with` block). TypeScript also takes `{ signal }`.

Streams reconnect by themselves after a network drop and resume exactly where they were. If the Controller can't
resume (the Computer's journal was reset), the stream yields one `gap` event and carries on live; pass
`onGap: "throw"` (`on_gap="throw"`) to raise `A12sGapError` instead. Events were lost either way: resync with
`status()`.

To replay one Turn from its start: `events("analyst", { turn: r.id })` (`turn=r.id`). To resume a stream you
saved: `{ after: lastSeq }` (`after=last_seq`).

## In the browser

The Controller listens on loopback with the owner token, so a web page can't call it directly. `a12s gateway` sits
in between: it serves your page, serves the TypeScript SDK at `/a12s.js`, and forwards `/v1/` to the Controller
with a scoped token the page never sees.

```sh
a12s gateway                     # the built-in dashboard: every Computer, live events, a send box
a12s gateway -- npm run dev      # your dev server (Vite, Next.js, …) behind the gateway, hot reload included
a12s gateway --dev http://localhost:5173   # a dev server that is already running
a12s gateway --web ./dist        # a built app (a directory of files)
```

Each run makes a new key and opens `http://127.0.0.1:7012/?key=…` in your browser (`--no-open` just prints it). The
gateway trades the key for an HttpOnly, SameSite=Strict cookie and drops it from the address bar; `/v1/` and
`/a12s.js` need that cookie, so another user or program on the machine can't use the Fleet through the gateway. A
script or test passes the key as a bearer token instead: `connect({ url: "http://127.0.0.1:7012", token: key })`.
Opened without the key, the gateway shows a page that says to use the printed link.

With a dev server, the gateway forwards every request except `/v1/` and
`/a12s.js` to it, websockets included, so the page and the API share one origin. It finds the dev server's port in
the URL it prints, or takes `--dev-port`. See [Recipes: A web app](recipes.md#a-web-app-react-vite-any-dev-server).

```html
<script type="module">
  import { connect } from "/a12s.js";
  const a12s = await connect();                       // same origin, no token
  for await (const ev of a12s.events()) console.log(ev.computer, ev.type);
</script>
```

The page may use `status`, `events`, `send` and `inbox` on every Computer; narrow that with
`--allow status,events` and `--computers lead,worker1`. The gateway prints the scope in one line when it starts:
`the page may read status, follow events, send Messages, read and manage inboxes, on every Computer`. Each gateway has its own token, `gateway-<port>`, so Messages
from the page arrive `via` `token:gateway-7012`. It is revoked when the gateway stops; one left by a killed gateway is
removed when the next gateway starts. Several gateways can serve one Fleet on different `--port`s.

The gateway answers only `127.0.0.1` and `localhost`, needs its key, and refuses writes from other origins, so other
sites open in the same browser can't use it. It is for this machine: a non-loopback `--addr` (`0.0.0.0`, a LAN
address) or a request for any other host name is refused, with the fix below.

### Serve other people

The gateway has no users, only one key. To put an app in front of other people, run your own server next to the
Fleet: it logs people in, decides what each may do, and calls a12s with a scoped token from server code, never the
browser.

```sh
a12s token create web --actions send,status --computers lead
```

```ts
// your server, on the Fleet's machine, behind your login
const a12s = await connect({ url: "http://127.0.0.1:52011", token: process.env.A12S_TOKEN });
app.post("/ask", requireLogin, async (req, res) => res.json(await a12s.send("lead", { body: req.body.q, sender: req.user.id })));
```

The Controller's port is in `~/.a12s/<fleet>/controller.json`; Messages arrive `via="token:web"`, with your `sender`.

With a bundler, `npm install @coltonkirsten/a12s` and `import { connect } from "@coltonkirsten/a12s"` work the same way:
in a browser, `connect()` talks to the page's own origin.

## Everything else

| TypeScript | Python | What it does |
|---|---|---|
| `status()` | `status()` | Every Computer and Service: `phase`, `session`, `activeTurn`, `inbox`, `power`, warnings. |
| `computer(name)` | `computer(name)` | One Computer's status. |
| `inbox(name, { state, bodies })` | `inbox(name, state=…, bodies=True)` | Pending Messages (`state: "all"` includes ended ones). |
| `message(name, id)`, `withdraw(name, id)` | the same | One Message; take back a pending one. |
| `stop(name, turn?)` | `stop(name, turn=None)` | Interrupt a Turn (default: the active one). |
| `sleep(name)`, `wake(name)` | the same | Stop a Computer and keep its disks and session; start it again. |
| `services()`, `service(name)` | the same | Service status. |
| `createToken({ name, actions, computers })` | `create_token(name, actions, computers)` | A scoped token for another program (owner only). |
| `putFleet(req)` | `put_fleet(req)` | What `a12s up` sends. Most apps leave this to the CLI. |

## Errors

A refused call raises `A12sError` with the HTTP `status`, a stable `code` and usually a `fix` line, which is also in
the message.

```ts
try { await a12s.send("lead", "…"); }
catch (err) { if (err instanceof A12sError && err.code === "inbox_full") { /* back off */ } else throw err; }
```

```python
try:
    client.send("lead", "…")
except a12s.A12sError as err:
    if err.code != "inbox_full":
        raise
```

The codes are listed in [HTTP API](api.md#errors). Streams also raise `A12sStreamError` for a malformed event and,
with `onGap: "throw"`, `A12sGapError`.

## Least privilege

The owner token can do anything. Give another program a scoped token instead:

```sh
a12s token create dashboard --actions status,events --computers '*'
```

Then `connect({ url, token })` / `a12s.connect(url=…, token=…)`. Actions: `send`, `inbox`, `status`, `events`,
`power`, `stop`. A token with `send` sees only its own Messages.
