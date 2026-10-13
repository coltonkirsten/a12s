# SDK

The SDK is how a program works with a running Fleet: create agent computers, send them work, follow what they do,
read the result. There is one for TypeScript (`@coltonkirsten/a12s` on npm) and one for Python (`a12s` on PyPI), with
the same objects and methods (camelCase in TypeScript, snake_case in Python).

```sh
npm install @coltonkirsten/a12s   # Node 20+, Bun, Deno, browsers
pip install a12s                # Python 3.10+ (uv add a12s)
```

```python
fleet = a12s.Fleet()            # connects to the running fleet (or url=, token=)
hello = fleet.agent_computers.create("hello", instructions="Be helpful")
print(hello.send("hi").wait().text)
hello.remove()
```

```ts
const fleet = await Fleet.connect();
const hello = await fleet.agentComputers.create("hello", { instructions: "Be helpful" });
console.log((await hello.send("hi").wait()).text);
await hello.remove();
```

A `Fleet` hands out objects: `fleet.agent_computers` (`fleet.agentComputers`) gives `AgentComputer`s,
`fleet.services` gives `Service`s, and `fleet.tokens` manages scoped tokens. You call methods on the objects.

## Connect

```ts
import { Fleet } from "@coltonkirsten/a12s";
const fleet = await Fleet.connect();
```

```python
import a12s
fleet = a12s.Fleet()
```

The Fleet is found the way the CLI finds it: the `fleet.yaml` in the current directory or a parent (or
`$A12S_FLEET`), else the only Fleet running on this machine. It connects as the owner, with the
`~/.a12s/<fleet>/controller.json` that `a12s up` writes. A Fleet has to be running (`a12s up`) first. Other ways in:

| You have | TypeScript | Python |
|---|---|---|
| Several Fleets running | `Fleet.connect({ fleet: "myproject" })` | `a12s.Fleet(fleet="myproject")` |
| A scoped token or another URL | `Fleet.connect({ url, token })` | `a12s.Fleet(url=…, token=…)` |
| This machine's Fleet under a scoped token | `Fleet.connect({ token })` | `a12s.Fleet(token=…)` |
| Environment | `$A12S_URL` and `$A12S_TOKEN` | the same |
| A web page | `a12s gateway -- npx vite` (or `--web dist` when built), then `Fleet.connect()` in the page ([In the browser](#in-the-browser)) | |

In Python, `with a12s.Fleet() as fleet:` closes the connection at the end (or call `fleet.close()`).
`a12s.AsyncFleet()` has the same objects and methods: `await` them, and use `async for` and `async with` for streams
(`await fleet.aclose()`).

## Agent computers

### Create

`create` takes a name and any [agent computer field](fleet-file.md#agent-computer-fields) of the Fleet file:
keyword arguments in Python, an object in TypeScript. Nested keys are the Fleet file's own (`sleepAfterIdle`,
`maxWakesPerHour`) in both languages. Whatever you leave out comes from the Fleet's `defaults`, so with harness, image and
credentials in `defaults` a name and instructions are enough. It returns once the agent computer is up.

```ts
const coder = await fleet.agentComputers.create("coder", {
  instructions: "You write and run Python. Answer with the output.",
  files: { ".claude/skills/style/SKILL.md": "…" },
  egress: { allow: ["pypi.org", "files.pythonhosted.org"] },
  power: { sleepAfterIdle: "30m" },
});
```

```python
coder = fleet.agent_computers.create(
    "coder",
    instructions="You write and run Python. Answer with the output.",
    files={".claude/skills/style/SKILL.md": "…"},
    egress={"allow": ["pypi.org", "files.pythonhosted.org"]},
    power={"sleepAfterIdle": "30m"},
)
```

- `instructions` becomes `CLAUDE.md` (Claude Code) or `AGENTS.md` (Codex) in its home; `files` are more text files
  there, by path. Together at most 1 MiB. A file that starts with `#!` is executable.
- `security.seccomp` is the profile's JSON object itself (in the Fleet file it is a path).
- A name that is taken fails with `A12sError` code `exists`; pass `replace: true` (`replace=True`) to overwrite it.
- A field that neither the spec nor `defaults` sets (harness, image) fails with `missing_field`. A credential the
  Controller holds no value for doesn't fail: `create` returns with a warning (`console.warn`, `A12sWarning`) and the
  agent computer waits in `Provisioning` (`status.waiting`: `{credentials, fix}`) until the value is set, then
  starts. `a12s creds set` or
  [`fleet.credentials.set`](#services-tokens-and-credentials) sets one.
- A grant may name an agent computer that doesn't exist yet (the write warns), so two that send to each other can be
  created one after the other.

### Get, list, update, remove

```ts
const lead = await fleet.agentComputers.get("lead");       // throws NotFound if there is none
const all = await fleet.agentComputers.list();
console.log(lead.name, lead.status.phase, lead.spec, lead.effective);
await lead.update({ instructions: "…", power: null });     // null: back to the default
await lead.reload();
await coder.remove();
```

```python
lead = fleet.agent_computers.get("lead")                   # raises a12s.NotFound if there is none
everyone = fleet.agent_computers.list()
print(lead.name, lead.status.phase, lead.spec, lead.effective)
lead.update(instructions="…")
lead.reload()
coder.remove()
```

- `spec` is the agent computer as written, `effective` has the defaults merged in, and `status` is its phase,
  session, inbox and power. All three are a snapshot from the last `create`, `get`, `reload` or `update`.
- `update` changes only the fields you pass (a JSON merge patch: maps merge, lists and values replace, `null` removes
  the field so the default applies). An idle or asleep agent computer takes it at once; a busy one at the end of its
  Turn. The session continues. Changed files are written again; other files in its home stay.
- `remove` stops it and removes it from the Fleet. Its drives and inbox are kept (`a12s status` lists them as
  orphans).
- Each call changes only that one agent computer. Two programs updating the same one: the last write wins.

The Fleet file and your code share one Fleet. `a12s up` makes the Fleet match the file again: it removes agent
computers your code created and undoes changes it made, without asking. To keep them, run `a12s export` first
([CLI](cli.md#keeping-changes-made-from-code)). Keep long-lived roles in `fleet.yaml`
and create the short-lived ones (one per task, user or branch) from code.

## Send work, get the answer

```ts
const analyst = await fleet.agentComputers.get("analyst");
const r = await analyst.send("Summarize the open issues in three bullets.").wait();
console.log(r.status, r.text);
```

```python
analyst = fleet.agent_computers.get("analyst")
r = analyst.send("Summarize the open issues in three bullets.").wait()
print(r.status, r.text)
```

`send()` puts a Message in the agent computer's inbox and returns the queued Message (`id`, `state`, …). Its
`wait()` waits until a Turn delivers the Message, follows that Turn and returns when it ends; if the agent computer
is busy, the Message waits its turn. The result is a [Turn](#turns) plus `message`. `onEvent` (`on_event`) gets that
Turn's events as they happen. `wait()` is keyed on the Message, so it can't miss the Turn: call it right away or
later, even after the Turn ended.

```ts
const m = await analyst.send("Summarize the open issues.");   // m.id, m.state
// …
const r = await m.wait({ onEvent: (ev) => ev.type === "text.delta" && process.stdout.write(ev.data.text) });
```

```python
m = analyst.send("Summarize the open issues.")   # m.id, m.state
# …
r = m.wait(on_event=lambda ev: print(ev.data.get("text", ""), end=""))
# AsyncFleet: r = await lead.send("…").wait(), or m = await lead.send("…") then r = await m.wait()
```

A failed Turn is returned with status `failed`. A Message that is withdrawn, expired or replaced before delivery,
or waits in a held inbox, raises `A12sError` with that code.

Without `wait()`, `send()` hands off work and moves on, for when the answer arrives some other way (from another
agent computer, say):

```ts
await analyst.send("Run the nightly audit.");
await analyst.send("…", { sender: "billing", metadata: { ticket: "T-42" }, expires: 3600 });
```

```python
analyst.send("Run the nightly audit.")
analyst.send("…", sender="billing", metadata={"ticket": "T-42"}, expires=3600)
```

| Option (TS / Python) | Meaning |
|---|---|
| `sender` | Your label for the sender, shown to the agent as `from`. Who really sent it is recorded as `via`. |
| `metadata` | Up to 16 string pairs, shown to the agent. |
| `fresh` | Start a new session (conversation) with this Message. |
| `expires` | Seconds to wait for delivery; `0` means now or never. |
| `replaceKey` / `replace_key` | Replace your pending Message with the same key, if it hasn't been delivered yet. |

Bodies are at most 64 KiB. The agent treats them as data from the sender, not as instructions that override its
role: put instructions in its `instructions`.

## Follow what happens

### Turns

`fleet.turns()` yields each Turn as it finishes, from one agent computer, several, or (no argument) all of them;
`agent.turns()` follows that one:

```ts
for await (const t of fleet.turns(["lead", "worker1"])) {
  console.log(t.agentComputer, t.status, t.messages.map((m) => m.via), t.text);
}
```

```python
for t in fleet.turns(["lead", "worker1"]):
    print(t.agent_computer, t.status, [m.via for m in t.messages], t.text)
```

A Turn has `agentComputer` / `agent_computer`, `id`, `status`
(`completed`, `failed` or `interrupted`), `text` (everything the agent said), `error`, `messages` (the Messages it
delivered, each with `id`, `sender` and `via`), `tools` (each tool call with `name`, `input`, `output`, `isError` /
`is_error`), `usage`, `tokens`, `startedAt` / `started_at` and `finishedAt` / `finished_at`. `error` is a string;
`input`, `output` and `usage` are the harness's JSON as is (a dict in Python). `tokens` (`TokenUsage`) is the same
for every harness: `input` (not counting cached input), `output`, `cacheRead` / `cache_read`, `cacheWrite` /
`cache_write`.

Turns that already ended: `agent.turns({ limit: 20 })` (`agent.turns(limit=20)`) returns its last finished Turns,
newest first, up to 200. The Controller keeps them, so this works while nothing was watching and while the agent
computer sleeps.
What an agent sends to another agent computer is in `tools`: a call named `mcp__a12s__send` whose `input` has
`computer` and `body`.

### Events

`events()` is the same stream before it's folded into Turns: every event as it happens. `fleet.events()` has every
agent computer (or those named) and the Fleet's own events (`fleet.changed`, `agent_computer.phase`,
`inbox.changed` with the inbox's summary, and `service.phase` for Services, matched by the Service's name);
`agent.events()` follows one. Every `events()` and `turns()` of one Fleet object shares one connection, however many
agent computers they watch.

```ts
for await (const ev of fleet.events()) {
  if (ev.type === "text.delta") process.stdout.write(ev.data.text);
  if (ev.type === "tool.started") console.log(`\n[${ev.agentComputer}] ${ev.data.name}`);
}
```

```python
for ev in fleet.events():
    if ev.type == "text.delta":
        print(ev.data["text"], end="", flush=True)
    if ev.type == "tool.started":
        print(f"\n[{ev.agent_computer}] {ev.data['name']}")
```

In TypeScript, events are a typed union: narrow on `ev.type` and `ev.data` has the right shape. In Python,
`ev.data` is the event's JSON as a dict. The types and their data are in [Concepts](concepts.md#event-journal).

### Don't miss the start

Streams start live. For the Turn of your own Message, `send(...).wait()` is enough. To see everything a piece of
work causes on other agent computers, start watching, wait until the stream is connected, then send:

```ts
const turns = fleet.turns(["lead", "worker1", "worker2"]);
await turns.ready();
await lead.send("Plan the launch.");
for await (const t of turns) { /* … */ }
```

```python
with fleet.turns(["lead", "worker1", "worker2"]) as turns:   # entering waits until it's connected
    lead.send("Plan the launch.")
    for t in turns:
        ...
```

An agent computer that is asleep counts as connected; its events start when it wakes. To end a stream, `break`, or
call `close()` (Python: leaving the `with` block). TypeScript also takes `{ signal }`.

Streams reconnect by themselves after a network drop and resume where they were. If the Controller can't resume
(it restarted, or the stream fell too far behind), the stream yields one `gap` event and carries on live; pass
`onGap: "throw"` (`on_gap="throw"`) to raise `A12sGapError` instead. Events were lost either way: resync with
`fleet.status()` and `agent.turns({ limit })`.

To replay one Turn from its start: `analyst.events({ turn: r.id })` (`turn=r.id`). The stream ends after its
`turn.finished`.

## The whole Fleet

`fleet.get()` returns the Fleet as written (`spec`, a `FleetSpec` with `defaults`, `agent_computers`, `services`,
`drives`) and its `version`. `fleet.replace(fn)` reads it, lets `fn` change it (in place, or by returning a new
one) and writes it back only if nobody wrote in between; otherwise it reads again and retries, up to 5 times.

```ts
await fleet.replace((f) => {
  f.defaults!.power = { sleepAfterIdle: "15m" };
  f.agent_computers.lead = { instructions: "…", grants: { send: ["worker1"] } };
  f.agent_computers.worker1 = { instructions: "…", grants: { send: ["lead"] } };
});
```

```python
def edit(f):
    f.defaults.update(power={"sleepAfterIdle": "15m"})
    f.agent_computers.update(
        lead={"instructions": "…", "grants": {"send": ["worker1"]}},
        worker1={"instructions": "…", "grants": {"send": ["lead"]}},
    )

fleet.replace(edit)
```

Every change adds one to the version. The HTTP API also checks a write without making it (`?dryRun=true`); see
[HTTP API](api.md#changing-the-fleet).

## Services, tokens and credentials

`fleet.services` has `create`, `get` and `list`; a `Service` has `reload`, `update`, `sleep`, `wake` and `remove`,
like an agent computer. `create` takes the [Service fields](services.md) of the Fleet file.

`fleet.tokens.create(name, …)` makes a [scoped token](#least-privilege) for another program (owner only); the secret
is in the result once. With a prefix it is a prefix token; with actions, a narrow one. `list()` and `remove(name)`
manage them.

```ts
const app = await fleet.tokens.create("bizapp", { prefix: "biz-" });
const tok = await fleet.tokens.create("dashboard", { actions: ["status", "events"], agentComputers: ["lead"] });
```

```python
app = fleet.tokens.create("bizapp", prefix="biz-")
tok = fleet.tokens.create("dashboard", actions=["status", "events"], agent_computers=["lead"])
```

`fleet.credentials` stores credential values in the Controller by name (the owner, or a prefix token for names
inside its prefix). `set(name, value)` stores one;
agent computers and Services that name it get the new value at once. `list()` returns a `CredentialInfo` per
credential the Fleet names or the Controller stores: `name`, `set`, `usedBy` (`used_by`) and `updatedAt`
(`updated_at`, when the value last changed), never the value.
`remove(name)` fails with `in_use` while an agent computer or Service names it. See
[Credentials](credentials.md#from-code-and-the-api).

```ts
await fleet.credentials.set("stripe-test", key);
const coder = await fleet.agentComputers.create("coder", { credentials: ["anthropic", "stripe-test"] });
```

```python
fleet.credentials.set("stripe-test", key)
coder = fleet.agent_computers.create("coder", credentials=["anthropic", "stripe-test"])
```

## In the browser

The Controller listens on loopback with the owner token, so a web page can't call it directly. `a12s gateway` sits
in between: it serves your page and forwards `/v1/` to the Controller with a scoped token the page never sees. The
page imports `@coltonkirsten/a12s` through a bundler such as Vite.

```sh
a12s gateway -- npm run dev      # your dev server (Vite, Next.js, …) behind the gateway, hot reload included
a12s gateway --dev http://localhost:5173   # a dev server that is already running
a12s gateway --web dist          # a built app (Vite's npm run build output)
```

Each run makes a new key and opens `http://127.0.0.1:7012/?key=…` in your browser (`--no-open` just prints it). The
gateway trades the key for an HttpOnly, SameSite=Strict cookie and drops it from the address bar; `/v1/` needs
that cookie, so another user or program on the machine can't use the Fleet through the gateway. A
script or test passes the key as a bearer token instead: `Fleet.connect({ url: "http://127.0.0.1:7012", token: key })`.
Opened without the key, the gateway shows a page that says to use the printed link.

With a dev server, the gateway forwards every request except `/v1/` to it, websockets included, so the page and the API share one origin. It finds the dev server's port in
the URL it prints, or takes `--dev-port`. See [Recipes: A web app](recipes.md#a-web-app-react-vite-any-dev-server).

```ts
import { Fleet } from "@coltonkirsten/a12s";
const fleet = await Fleet.connect();                  // same origin, no token
for await (const ev of fleet.events()) console.log(ev.agentComputer, ev.type);
```

The page may use `status`, `events`, `send` and `inbox` on every agent computer; narrow that with
`--allow status,events` and `--agent-computers lead,worker1`. Creating, changing and removing agent computers is
owner-only, so a page can't. The gateway prints the scope in one line when it starts:
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
a12s token create web --actions send,status --agent-computers lead
```

```ts
// your server, on the Fleet's machine, behind your login
const fleet = await Fleet.connect({ url: "http://127.0.0.1:52011", token: process.env.A12S_TOKEN });
const lead = await fleet.agentComputers.get("lead");
app.post("/ask", requireLogin, async (req, res) => res.json(await lead.send(req.body.q, { sender: req.user.id })));
```

The Controller's port is in `~/.a12s/<fleet>/controller.json`; Messages arrive `via="token:web"`, with your `sender`.
A server that creates agent computers for its users uses a prefix token
(`a12s token create web --prefix web-`) and names them `web-…`; keep it on the server.

With a bundler, `npm install @coltonkirsten/a12s` and `import { Fleet } from "@coltonkirsten/a12s"` work the same
way: in a browser, `Fleet.connect()` talks to the page's own origin.

## Everything else

| On | TypeScript | Python | What it does |
|---|---|---|---|
| Fleet | `status()` | `status()` | Every agent computer (`agentComputers` / `agent_computers`) and Service: `phase`, `session`, `activeTurn`, `inbox`, `power`, warnings. |
| AgentComputer | `inbox({ state, bodies })` | `inbox(state=…, bodies=True)` | Pending Messages (`state: "all"` includes ended ones). |
| AgentComputer | `message(id)`, `withdraw(id)` | the same | One Message; take back a pending one. |
| AgentComputer | `stop(turn?)` | `stop(turn=None)` | Interrupt a Turn (default: the active one). |
| AgentComputer | `sleep({ hold? })`, `wake()` | `sleep(hold=)`, `wake()` | Stop it and keep its disks, session and inbox; start it again. A sender listed in its wakeOnMessage wakes a plain sleep; `hold` keeps it asleep until `wake()`. |
| AgentComputer | `hold({ reason? })`, `resume()` | `hold(reason=)`, `resume()` | Hold delivery (Messages still queue); release it. |

In TypeScript the spec types are `AgentComputerSpec` and `FleetSpec`, and `fleet.client.request(method, path, body?)`
calls a route that has no method here, with the Fleet's URL and token.

## Errors

A refused call raises `A12sError` with the HTTP `status`, a stable `code` and usually a `fix` line, which is also in
the message. A 404 is `NotFound`, a subclass.

```ts
try { await lead.send("…"); }
catch (err) { if (err instanceof A12sError && err.code === "inbox_full") { /* back off */ } else throw err; }
```

```python
try:
    lead.send("…")
except a12s.A12sError as err:
    if err.code != "inbox_full":
        raise
```

The codes are listed in [HTTP API](api.md#errors). Streams also raise `A12sStreamError` for a malformed event and,
with `onGap: "throw"`, `A12sGapError`. A failed Turn is not an error: `wait()` returns it with status `failed`.

## Least privilege

The owner token can do anything. Give another program a scoped token instead, and connect with
`Fleet.connect({ url, token })` / `a12s.Fleet(url=…, token=…)`:

```sh
a12s token create bizapp --prefix biz-                               # an app's own lane
a12s token create dashboard --actions status,events --agent-computers '*'   # read-only
```

- A **prefix token** may do everything the owner can with agent computers, Services and credentials whose names start
  with its prefix: `fleet.agentComputers.create("biz-a", …)`, send, events, sleep, remove. Lists show only those
  names; anything outside, or a spec whose `grants.send` or `uses` (also from `defaults`) point outside,
  raises `out_of_prefix`. It can't call `fleet.get()`, `fleet.replace()` or `fleet.tokens`, or set
  `security.seccomp` (`forbidden`). See
  [Security: Tokens](security.md#tokens).
- A **narrow token** may make only its actions (`send`, `inbox`, `status`, `events`, `power`, `stop`) on its agent
  computers. A token with `send` sees only its own Messages. Creating, changing and removing agent computers, Services
  and tokens, and `fleet.get()` and `fleet.replace()`, need the owner token or a prefix token where allowed.
