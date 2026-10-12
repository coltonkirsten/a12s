# Recipes

Each recipe is complete: copy it, run it, change it. They assume a12s is installed and `a12s creds set anthropic`
is done ([Quickstart](../README.md#quickstart-5-minutes)), and use the TypeScript or Python SDK ([SDK](sdk.md)).

- [Lead and workers](#lead-and-workers)
- [A live dashboard in the browser](#a-live-dashboard-in-the-browser)
- [Change an agent's instructions](#change-an-agents-instructions)
- [Give agents tools and network access](#give-agents-tools-and-network-access)
- [Run work on a schedule](#run-work-on-a-schedule)
- [Start work from a GitHub webhook](#start-work-from-a-github-webhook)
- [Let another program in](#let-another-program-in)
- [Any language: the HTTP API](#any-language-the-http-api)
- [See what's going on](#see-whats-going-on)

## Lead and workers

A lead splits a task, hands the parts to two workers, and combines their answers. The agents talk through their
inboxes; your app sends the task and watches. This is what the
[starter template](https://github.com/coltonkirsten/a12s-starter) runs.

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

defaults:                                 # every Computer gets these
  harness: { kind: claude-code, version: 2.1.280, model: haiku }
  image: slim
  credentials: [anthropic]

computers:
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
import { connect } from "a12s";

const a12s = await connect();
const turns = a12s.turns(["lead", "worker1", "worker2"]);
await turns.ready();                                   // watch first, so nothing is missed
await a12s.send("lead", process.argv[2] ?? "Two facts about octopuses: one about hearts, one about arms.");

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

client = a12s.connect()
task = sys.argv[1] if len(sys.argv) > 1 else "Two facts about octopuses: one about hearts, one about arms."

with client.turns(["lead", "worker1", "worker2"]) as turns:   # entering waits until it's watching
    client.send("lead", task)
    for t in turns:
        print(f"[{t.computer}] {t.text.strip()}")
        if t.computer == "lead" and "FINAL ANSWER:" in t.text:
            print("\n" + t.text.split("FINAL ANSWER:", 1)[1].strip())
            break
```

```sh
a12s up && npx tsx app.ts       # or: python app.py
```

To see who handed work to whom, look at `t.messages`: each has `via` (`computer:worker1`, `local`, …) and `sender`.

## A live dashboard in the browser

No code: `a12s gateway` serves a built-in dashboard at http://127.0.0.1:7012 with every Computer, their events as
they stream, and a box to send a Message.

Your own page: put it in a directory and serve that instead. The gateway also serves the SDK at `/a12s.js`, and
the page needs no token or build step.

```html
<!-- web/index.html -->
<!doctype html>
<meta charset="utf-8">
<title>Team</title>
<pre id="log"></pre>
<script type="module">
  import { connect } from "/a12s.js";

  const a12s = await connect();
  const log = (line) => (document.getElementById("log").textContent += line + "\n");

  for await (const ev of a12s.events()) {          // every Computer, merged
    if (ev.type === "turn.started") log(`${ev.computer}: started (from ${ev.data.inbox?.messages.map((m) => m.via).join(", ")})`);
    if (ev.type === "tool.started") log(`${ev.computer}: → ${ev.data.name}`);
    if (ev.type === "turn.finished") log(`${ev.computer}: ${ev.data.status}`);
  }
</script>
```

```sh
a12s gateway --web web          # http://127.0.0.1:7012
```

To show whole answers instead of events, use `a12s.turns()` the same way. The page can send work with
`a12s.send(...)`; those Messages arrive `via="token:gateway"`. Limit what the page may do with
`--allow status,events` (read-only) and `--computers lead`.

## Change an agent's instructions

Edit its preset and apply:

```sh
$EDITOR presets/lead/CLAUDE.md
a12s up                         # "updated lead": the new files are copied at its next idle moment
```

The Computer restarts between Turns, never in the middle of one, and keeps its conversation. To also start a fresh
conversation, send the next Message with `fresh: true` (`a12s prompt lead --new "…"`).

A preset can hold anything Claude Code reads from the home directory: `CLAUDE.md`, `.claude/skills/`,
`.claude/settings.json`, `.mcp.json`. Its files overwrite the agent's copies; other files on the home drive stay.

## Give agents tools and network access

Every Computer is deny-by-default on the network. Allow hosts per Computer (or in `defaults`):

```yaml
computers:
  worker1:
    egress: { allow: [github.com, api.github.com, registry.npmjs.org, pypi.org, files.pythonhosted.org] }
```

The agent can install tools itself into its home drive (`pip install --user`, `npm install`, a binary in
`~/bin`); they survive sleep. For tools every Computer should have from the start, build an image `FROM` the a12s
base ([Extending: a custom agent image](extending.md#a-custom-agent-image)). For a secret the agent shouldn't hold,
put it in a Service ([Services](services.md)).

## Run work on a schedule

Add the reference clock Service and let it send to a Computer:

```yaml
services:
  clock:
    image: ghcr.io/coltonkirsten/a12s/svc-cron:0.2.0     # match your a12s version
    grants: { send: [lead] }
    config:
      jobs:
        - { name: standup, cron: "0 9 * * 1-5", to: lead, body: "Summarize what changed yesterday.", replace: standup }
```

`a12s init <dir> --with cron` writes this for you. `replace` keeps at most one pending standup if lead is busy.
To let the clock wake a sleeping Computer, add `power: { wakeOnMessage: { from: [service:clock] } }` to it
([Inbox and power](inbox.md#power-policy)).

## Start work from a GitHub webhook

```yaml
services:
  hooks:
    image: ghcr.io/coltonkirsten/a12s/svc-webhook:0.2.0
    credentials: [webhook-secret]                  # a12s creds set webhook-secret
    grants: { send: [lead] }
    publish: ["127.0.0.1:8787:8080"]
    config:
      routes: [{ path: /github, to: lead, secretFrom: webhook-secret }]
```

Point the GitHub webhook (through your tunnel) at `/github` on port 8787. Signed deliveries become Messages to lead
with `metadata: {source: github, event}`; unsigned ones are refused.

## Let another program in

The owner token can do everything. Give a script or service a scoped token:

```sh
a12s token create ci --actions send,events --computers lead
```

```ts
const a12s = await connect({ url: "http://127.0.0.1:52011", token: process.env.A12S_TOKEN });
```

```python
client = a12s.connect(url="http://127.0.0.1:52011", token=os.environ["A12S_TOKEN"])
```

The port is in `~/.a12s/<fleet>/controller.json`. Messages it sends arrive `via="token:ci"`.

## Any language: the HTTP API

```sh
CF=~/.a12s/team/controller.json
URL=http://127.0.0.1:$(jq .port $CF); TOKEN=$(jq -r .token $CF)

curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"body": "Ping", "idempotencyKey": "ping-1"}' $URL/v1/computers/lead/inbox
curl -sN -H "Authorization: Bearer $TOKEN" $URL/v1/computers/lead/events      # live events (SSE)
```

Routes, JSON and the event-stream format: [HTTP API](api.md).

## See what's going on

```sh
a12s status                     # every Computer: phase, inbox, power, warnings
a12s logs lead --follow         # one Computer's events as text
a12s gateway                    # all of them, live, in the browser
a12s doctor                     # what's broken, one line each, with the fix
a12s inbox lead --bodies        # what's waiting
```

An agent that doesn't call `send` usually lacks the grant: `send` only exists for Computers listed in its
`grants.send`. A Message that never gets delivered usually means the inbox is held (`a12s inbox lead` says why;
`a12s inbox resume lead` releases it) or the Computer is asleep without a wake policy (`a12s wake lead`).
