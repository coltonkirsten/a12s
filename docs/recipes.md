# Recipes

Each recipe is complete: copy it, run it, change it. They assume a12s is installed and `a12s creds set anthropic`
is done ([Quickstart](../README.md#quickstart)), and use the TypeScript or Python SDK ([SDK](sdk.md)).

- [An agent computer per task](#an-agent-computer-per-task)
- [Lead and workers](#lead-and-workers)
- [A web app (React, Vite, any dev server)](#a-web-app-react-vite-any-dev-server)
- [A live dashboard in the browser](#a-live-dashboard-in-the-browser)
- [Change an agent's instructions](#change-an-agents-instructions)
- [Give agents tools and network access](#give-agents-tools-and-network-access)
- [Run work on a schedule](#run-work-on-a-schedule)
- [Start work from a webhook](#start-work-from-a-webhook)
- [Let another program in](#let-another-program-in)
- [Give an app its own lane](#give-an-app-its-own-lane)
- [Any language: the HTTP API](#any-language-the-http-api)
- [See what's going on](#see-whats-going-on)

## An agent computer per task

Create an agent computer for one piece of work, use it, and remove it. The Fleet file supplies what every agent
computer shares, in `defaults`:

```yaml
# fleet.yaml
apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: review }

defaults:
  harness: { kind: claude-code, version: 2.1.280, model: sonnet }
  image: slim
  credentials: [anthropic]
  egress: { allow: [github.com, api.github.com] }

agent_computers:
  triage:                                 # a long-lived one
    instructions: Sort incoming issues by area and urgency.
```

```ts
// review.ts — npx tsx review.ts https://github.com/you/repo/pull/7
import { Fleet } from "@coltonkirsten/a12s";

const fleet = await Fleet.connect();
const reviewer = await fleet.agentComputers.create(`review-${Date.now()}`, {
  instructions: "You review pull requests. Clone the repo, read the diff, answer with at most five findings.",
});
try {
  const r = await reviewer.prompt(`Review ${process.argv[2]}`);
  console.log(r.text);
} finally {
  await reviewer.remove();
}
```

```python
# review.py — python review.py https://github.com/you/repo/pull/7
import sys, time
import a12s

with a12s.Fleet() as fleet:
    reviewer = fleet.agent_computers.create(
        f"review-{int(time.time())}",
        instructions="You review pull requests. Clone the repo, read the diff, answer with at most five findings.",
    )
    try:
        print(reviewer.prompt(f"Review {sys.argv[1]}").text)
    finally:
        reviewer.remove()
```

Names are DNS labels of at most 40 characters. `remove()` keeps the home drive: `a12s drive ls` lists it as
orphaned, and `a12s drive rm home-<name>` deletes it. `a12s up` removes agent computers the file doesn't list, so
run it when no task is in flight.

## Lead and workers

A lead splits a task, hands the parts to two workers, and combines their answers. The agents talk through their
inboxes; your app sends the task and watches.

```text
team/
  fleet.yaml
  presets/lead/CLAUDE.md
  presets/worker/CLAUDE.md
  app.ts  or  app.py
```

```yaml
# fleet.yaml
apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: team }

defaults:                                 # every agent computer gets these
  harness: { kind: claude-code, version: 2.1.280, model: haiku }
  image: slim
  credentials: [anthropic]

agent_computers:
  lead:
    home: { preset: ./presets/lead }      # its CLAUDE.md: the role
    grants: { send: [worker1, worker2] }  # gives lead a send tool for these two
  worker1:
    home: { preset: ./presets/worker }
    grants: { send: [lead] }
  worker2:
    home: { preset: ./presets/worker }
    grants: { send: [lead] }
```

```markdown
<!-- presets/lead/CLAUDE.md -->
# You are lead

You coordinate `worker1` and `worker2` with your `send` tool. You never do the work yourself.

When a task arrives (any message not from worker1 or worker2):
1. Split it into two independent parts.
2. `send` one part to worker1 and the other to worker2. Ask for a short answer, sent back to `lead`.
3. Reply `DISPATCHED` and end your turn. Don't wait: their results arrive as new messages.

When a worker's result arrives (`via="computer:worker1"` or `computer:worker2`):
- If you now have both results for the task, reply `FINAL ANSWER:` followed by the combined answer.
- Otherwise reply `WAITING`.
```

```markdown
<!-- presets/worker/CLAUDE.md -->
# You are a worker

`lead` sends you one part of a task (`via="computer:lead"`). Do it, keep the answer short, and `send` it to `lead`
starting with `RESULT from <your name>:`. Then reply `SENT`.
```

Each result reaches lead in its own Turn. Lead's conversation continues between Turns, so it remembers the task and
what has arrived.

The app starts watching, sends the task, and stops at lead's final answer:

```ts
// app.ts — npx tsx app.ts "your task"   (or node app.ts on Node 24+)
import { Fleet } from "@coltonkirsten/a12s";

const fleet = await Fleet.connect();
const lead = await fleet.agentComputers.get("lead");
const turns = fleet.turns(["lead", "worker1", "worker2"]);
await turns.ready();                                   // watch first, so nothing is missed
await lead.send(process.argv[2] ?? "Two facts about octopuses: one about hearts, one about arms.", { fresh: true });

for await (const t of turns) {
  console.log(`[${t.computer}] ${t.text.trim()}`);
  const at = t.text.indexOf("FINAL ANSWER:");
  if (t.computer === "lead" && at >= 0) {
    console.log("\n" + t.text.slice(at + "FINAL ANSWER:".length).trim());
    break;
  }
}
```

```python
# app.py — python app.py "your task"
import sys
import a12s

fleet = a12s.Fleet()
lead = fleet.agent_computers.get("lead")
task = sys.argv[1] if len(sys.argv) > 1 else "Two facts about octopuses: one about hearts, one about arms."

with fleet.turns(["lead", "worker1", "worker2"]) as turns:    # entering waits until it's watching
    lead.send(task, fresh=True)
    for t in turns:
        print(f"[{t.computer}] {t.text.strip()}")
        if t.computer == "lead" and "FINAL ANSWER:" in t.text:
            print("\n" + t.text.split("FINAL ANSWER:", 1)[1].strip())
            break
```

```sh
a12s up && npx tsx app.ts       # or: python app.py
```

`fresh: true` starts lead on a new conversation for each task, so an earlier task's answer can't leak into this
one; the workers' replies then continue that new conversation. Leave it out when a task should build on the last.

To see who handed work to whom, look at `t.messages`: each has `via` (`computer:worker1`, `local`, …) and `sender`.
What an agent sent is in `t.tools`: a worker's `send` is the call named `mcp__a12s__send`, and `input.body` is its
result.

## A web app (React, Vite, any dev server)

This is the way to build a frontend on a Fleet. Run your dev server through the gateway: the page and the API share
one origin, so the page needs no token, no CORS setup and no proxy config, and hot reload keeps working.

```sh
npm create vite@latest web -- --template react-ts && cd web
npm install && npm install @coltonkirsten/a12s
a12s gateway -F ../fleet.yaml -- npm run dev    # opens http://127.0.0.1:7012/?key=…, not the dev server's own port
```

```tsx
// src/App.tsx
import { useEffect, useState } from "react";
import { Fleet } from "@coltonkirsten/a12s";

const fleet = await Fleet.connect();             // same origin as the gateway: no URL, no token

export default function App() {
  const [names, setNames] = useState<string[]>([]);
  const [sent, setSent] = useState("");
  useEffect(() => {
    fleet.agentComputers.list().then((all) => setNames(all.map((a) => a.name)));
  }, []);
  const send = async () => {
    const hello = await fleet.agentComputers.get("hello");
    setSent((await hello.send("Hi from the page")).id);
  };
  return (
    <main>
      <p>{names.join(", ")}</p>
      <button onClick={send}>Send</button>
      <p>{sent}</p>
    </main>
  );
}
```

`a12s gateway -- <command>` starts the command, waits for the URL it prints (`Local: http://localhost:5173/`), and
forwards everything except `/v1/` to it, websockets (Vite's hot reload) included. Ctrl-C stops both.
If your dev server doesn't print its URL, add `--dev-port 3000`. If it is already running, use
`a12s gateway --dev http://localhost:5173` instead. Next.js, webpack and other dev servers work the same way.

For production, `npm run build` and serve the output with `a12s gateway --web dist`.

## A live dashboard in the browser

No code: `a12s gateway` opens a built-in dashboard at http://127.0.0.1:7012 with every agent computer, their events as
they stream, and a box to send a Message.

Your own page: a Vite app that imports the npm package, run through the gateway. The page needs no token.

```sh
npm install @coltonkirsten/a12s vite
```

```html
<!-- index.html -->
<!doctype html>
<meta charset="utf-8">
<title>Team</title>
<pre id="log"></pre>
<script type="module" src="/main.js"></script>
```

```js
// main.js
import { Fleet } from "@coltonkirsten/a12s";

const fleet = await Fleet.connect();
const log = (line) => (document.getElementById("log").textContent += line + "\n");

for await (const ev of fleet.events()) {           // every agent computer, merged
  if (ev.type === "turn.started") log(`${ev.computer}: started (from ${ev.data.inbox?.messages.map((m) => m.via).join(", ")})`);
  if (ev.type === "tool.started") log(`${ev.computer}: → ${ev.data.name}`);
  if (ev.type === "turn.finished") log(`${ev.computer}: ${ev.data.status}`);
}
```

```sh
a12s gateway -- npx vite        # opens http://127.0.0.1:7012/?key=…
npx vite build && a12s gateway --web dist   # or serve the built app
```

To show whole answers instead of events, use `fleet.turns()` the same way. The page can send work with
`(await fleet.agentComputers.get("lead")).send(...)`; those Messages arrive `via="token:gateway-7012"` (the
gateway's port). Limit what the page may do with `--allow status,events` (read-only) and `--computers lead`.

## Change an agent's instructions

Edit its `instructions` (or its preset's `CLAUDE.md`) and apply:

```sh
$EDITOR fleet.yaml              # or presets/lead/CLAUDE.md
a12s up                         # "updated lead": the new files are written at its next idle moment
```

From code, for an agent computer your code owns:

```ts
await (await fleet.agentComputers.get("helper")).update({ instructions: "Answer in one sentence." });
```

```python
fleet.agent_computers.get("helper").update(instructions="Answer in one sentence.")
```

The agent computer restarts between Turns, never in the middle of one, and keeps its conversation. To also start a
fresh conversation, send the next Message with `fresh: true` (`a12s prompt lead --new "…"`).

`files` (or a preset directory) can hold anything Claude Code reads from the home directory: `.claude/skills/`,
`.claude/settings.json`, `.mcp.json`. They overwrite the agent's copies; other files on the home drive stay.

## Give agents tools and network access

Every agent computer is deny-by-default on the network. Allow hosts per agent computer (or in `defaults`):

```yaml
agent_computers:
  worker1:
    egress: { allow: [github.com, api.github.com, registry.npmjs.org, pypi.org, files.pythonhosted.org] }
```

The agent can install tools itself into its home drive (`pip install --user`, `npm install`, a binary in
`~/bin`); they survive sleep. For tools every agent computer should have from the start, build an image `FROM` the a12s
base ([Extending: a custom agent image](extending.md#a-custom-agent-image)). For a secret the agent shouldn't hold,
put it in a Service ([Services](services.md)).

## Run work on a schedule

The simplest clock is the host's own: `a12s send` enqueues and exits, whether the agent computer is busy or asleep.

```sh
# crontab -e
0 9 * * 1-5  /opt/homebrew/bin/a12s -F ~/team/fleet.yaml send lead "Summarize what changed yesterday." --replace standup
```

`--replace standup` keeps at most one pending standup if lead is busy. To let it wake a sleeping agent computer, add
`power: { wakeOnMessage: { from: [local] } }` to it ([Inbox and power](inbox.md#power-policy)).

To keep the schedule inside the Fleet (it runs wherever the Fleet runs, under its own grant), run a clock Service:
[Services: Write a Service with svckit](services.md#write-a-service-with-svckit) builds one, and the wake rule is
then `from: [service:clock]`.

## Start work from a webhook

Whatever receives the webhook sends the Message. From your own server, use the SDK with a token that may only send
to lead (`a12s token create hooks --actions send --agent-computers lead`, see [below](#let-another-program-in)), check
the sender's signature, then:

```ts
await lead.send(`GitHub ${event}: ${summary}`, { sender: "github", metadata: { source: "github", event } });
```

To run the receiver inside the Fleet instead, write a Service with `grants.send` and `publish:`
([Services](services.md#write-a-service-with-svckit)); the v0.2.1 source has a
[GitHub webhook bridge](https://github.com/coltonkirsten/a12s/tree/v0.2.1/services/svc-webhook) to copy.

## Let another program in

The owner token can do everything. Give a script or service a scoped token:

```sh
a12s token create ci --actions send,events --agent-computers lead
```

```ts
const fleet = await Fleet.connect({ url: "http://127.0.0.1:52011", token: process.env.A12S_TOKEN });
```

```python
fleet = a12s.Fleet(url="http://127.0.0.1:52011", token=os.environ["A12S_TOKEN"])
```

The port is in `~/.a12s/<fleet>/controller.json`. Messages it sends arrive `via="token:ci"`. This token can send
and watch; to let a program create its own agent computers, give it [a lane](#give-an-app-its-own-lane).

## Give an app its own lane

An app that creates and removes its own agent computers gets a prefix token. The owner makes it once:

```sh
a12s token create bizapp --prefix biz-        # printed once; store it as the app's A12S_TOKEN
```

The app connects with it and works on `biz-*` names like the owner would:

```ts
const fleet = await Fleet.connect({ url: "http://127.0.0.1:52011", token: process.env.A12S_TOKEN });
const sales = await fleet.agentComputers.create("biz-sales", {
  instructions: "You answer sales questions.",
  grants: { send: ["biz-lead"] },               // only biz-* names
});
await sales.send("Draft a reply to the Acme lead.");
```

```python
fleet = a12s.Fleet(url="http://127.0.0.1:52011", token=os.environ["A12S_TOKEN"])
sales = fleet.agent_computers.create("biz-sales", instructions="You answer sales questions.", grants={"send": ["biz-lead"]})
sales.send("Draft a reply to the Acme lead.")
```

It sees only `biz-*` agent computers, Services and credentials. Naming anything else, or inheriting a `defaults`
grant, use or drive outside `biz-`, fails with `out_of_prefix`; set the agent computer's own `grants` to override
the default. Its agent computers may use any credential the Fleet stores (here `anthropic`, from `defaults`), and
their egress and image are its call, so trust the app like the owner for them. See
[Security: Tokens](security.md#tokens).

## Any language: the HTTP API

```sh
CF=~/.a12s/team/controller.json
URL=http://127.0.0.1:$(jq .port $CF); TOKEN=$(jq -r .token $CF)

curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"body": "Ping", "idempotencyKey": "ping-1"}' $URL/v1/agent-computers/lead/inbox
curl -sN -H "Authorization: Bearer $TOKEN" $URL/v1/agent-computers/lead/events      # live events (SSE)
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'If-None-Match: *' \
  -d '{"instructions": "Answer in one sentence."}' $URL/v1/agent-computers/helper   # create one
```

Routes, JSON and the event-stream format: [HTTP API](api.md).

## See what's going on

```sh
a12s status                     # every agent computer: phase, inbox, power, warnings
a12s logs lead --follow         # one agent computer's events as text
a12s gateway                    # all of them, live, in the browser
a12s doctor                     # what's broken, one line each, with the fix
a12s inbox lead --bodies        # what's waiting
```

An agent that doesn't call `send` usually lacks the grant: `send` only exists for agent computers listed
in its `grants.send`. A Message that never gets delivered usually means the inbox is held (`a12s inbox lead` says why;
`a12s inbox resume lead` releases it) or the agent computer is asleep without a wake policy (`a12s wake lead`).
