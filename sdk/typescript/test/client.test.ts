import assert from "node:assert/strict";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";

import { mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { A12sClient, A12sError, A12sGapError, A12sStreamError } from "../src/index.js";
import { connect, fleetName } from "../src/node.js";

type Handler = (req: IncomingMessage, res: ServerResponse, body: string) => void;

async function fake(handler: Handler): Promise<{ client: A12sClient; close: () => void }> {
  const srv = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => handler(req, res, body));
  });
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
  const { port } = srv.address() as AddressInfo;
  return {
    client: new A12sClient({ url: `http://127.0.0.1:${port}/`, token: "tok", fetch }),
    close: () => {
      srv.closeAllConnections();
      srv.close();
    },
  };
}

const ev = (seq: number, type: string, turn: string, data: unknown) =>
  `id: ${seq}\ndata: ${JSON.stringify({ v: 1, computer: "lead", session: "s", turn, seq, ts: "", type, data })}\n\n`;

test("prompt: enqueue, wait for delivery, stream the turn across a reconnect", async () => {
  let eventsCalls = 0;
  const { client, close } = await fake((req, res, body) => {
    assert.equal(req.headers.authorization, "Bearer tok");
    if (req.method === "POST" && req.url === "/v1/computers/lead/inbox") {
      const m = JSON.parse(body);
      assert.equal(m.body, "split this");
      assert.ok(m.idempotencyKey, "the client always sends an idempotency key");
      res.writeHead(201, { "content-type": "application/json" }).end(JSON.stringify({ id: "m_1", seq: 1, state: "pending" }));
      return;
    }
    res.writeHead(200, { "content-type": "text/event-stream" });
    if (req.url === "/v1/computers/lead/inbox/m_1/events") {
      res.write(ev(1, "snapshot", "", { message: { id: "m_1", state: "pending" }, held: false }));
      res.write(": keepalive\n\n");
      res.end(ev(2, "delivered", "", { turn: "t_9" }));
      return;
    }
    assert.equal(req.url, "/v1/computers/lead/events?turn=t_9");
    eventsCalls++;
    if (eventsCalls === 1) {
      res.write("event: head\ndata: {\"seq\":0}\n\n");
      res.end(ev(10, "text.delta", "t_9", { text: "hel" }));
      return;
    }
    assert.equal(req.headers["last-event-id"], "10", "reconnect resumes after the last seq");
    res.write(ev(11, "text.delta", "t_9", { text: "lo" }));
    res.end(ev(12, "turn.finished", "t_9", { status: "completed", usage: {} }));
  });
  try {
    const seen: string[] = [];
    const r = await client.prompt("lead", "split this", { onEvent: (e) => seen.push(e.type) });
    assert.deepEqual([r.id, r.status, r.text, r.message.id], ["t_9", "completed", "hello", "m_1"]);
    assert.deepEqual(seen, ["text.delta", "text.delta", "turn.finished"]);
  } finally {
    close();
  }
});

test("errors carry the code and fix line", async () => {
  const { client, close } = await fake((_req, res) => {
    res.writeHead(403, { "content-type": "application/json" })
      .end(JSON.stringify({ code: "no_grant", message: "worker1 may not send to lead", fix: "add lead to grants.send" }));
  });
  try {
    await assert.rejects(client.send("lead", "x"), (e: unknown) => {
      assert.ok(e instanceof A12sError);
      assert.equal(e.status, 403);
      assert.equal(e.code, "no_grant");
      assert.match(e.message, /fix: add lead to grants.send/);
      return true;
    });
  } finally {
    close();
  }
});

const collect = async <T>(it: AsyncIterable<T>, n: number): Promise<T[]> => {
  const out: T[] = [];
  for await (const x of it) {
    out.push(x);
    if (out.length === n) break;
  }
  return out;
};

test("events: a gap is yielded as a typed event, then the stream goes on live", async () => {
  const { client, close } = await fake((req, res) => {
    assert.equal(req.headers["last-event-id"], "41");
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.write('event: gap\ndata: {"lastEventId":"41","head":7}\n\n');
    res.write(ev(8, "diagnostic", "", { message: "after gap" }));
  });
  try {
    const [gap, next] = await collect(client.events("lead", { after: 41 }), 2);
    assert.equal(gap!.type, "gap");
    assert.equal(gap!.computer, "lead");
    assert.deepEqual(gap!.data, { lastEventId: "41", head: 7, after: 41 });
    assert.equal(next!.seq, 8);
  } finally {
    close();
  }
});

test("events: onGap 'throw' throws A12sGapError", async () => {
  const { client, close } = await fake((_req, res) => {
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.write('event: gap\ndata: {"lastEventId":"41","head":7}\n\n');
  });
  try {
    await assert.rejects(collect(client.events("lead", { after: 41, onGap: "throw" }), 1), (e: unknown) => {
      assert.ok(e instanceof A12sGapError);
      assert.equal(e.after, 41);
      return true;
    });
  } finally {
    close();
  }
});

test("events: a malformed event throws instead of reconnecting forever", async () => {
  let calls = 0;
  const { client, close } = await fake((_req, res) => {
    calls++;
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.write(ev(1, "diagnostic", "", {}));
    res.write("id: 2\ndata: {not json\n\n");
  });
  try {
    await assert.rejects(collect(client.events("lead", { reconnectMs: 10 }), 5), (e: unknown) => {
      assert.ok(e instanceof A12sStreamError);
      assert.match(e.message, /malformed event after seq 1/);
      return true;
    });
    assert.equal(calls, 1);
  } finally {
    close();
  }
});

test("events: an error thrown into the stream propagates; a dropped connection reconnects", async () => {
  let calls = 0;
  const { client, close } = await fake((_req, res) => {
    calls++;
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.write(ev(calls, "diagnostic", "", {}));
    if (calls === 1) setTimeout(() => res.destroy(), 20); // a network drop mid-stream
  });
  try {
    const it = client.events("lead", { reconnectMs: 10 });
    assert.equal((await it.next()).value!.seq, 1);
    assert.equal((await it.next()).value!.seq, 2, "reconnected after the drop");
    assert.equal(calls, 2);
    await assert.rejects(it.throw(new Error("consumer bug")), /consumer bug/);
    assert.deepEqual(await it.next(), { done: true, value: undefined });
    assert.equal(calls, 2, "no reconnect after a consumer error");
  } finally {
    close();
  }
});

test("events: a non-network fetch failure is rethrown", async () => {
  const client = new A12sClient({
    url: "http://x",
    token: "t",
    fetch: (async () => {
      throw new RangeError("bad fetch impl");
    }) as unknown as typeof fetch,
  });
  await assert.rejects(collect(client.events("lead"), 1), RangeError);
});

test("connect: url, fleet name, nearest fleet.yaml, the only running Fleet", async () => {
  const home = await mkdtemp(join(tmpdir(), "a12s-"));
  const write = async (fleet: string, port: number) => {
    await mkdir(join(home, ".a12s", fleet), { recursive: true });
    await writeFile(join(home, ".a12s", fleet, "controller.json"), JSON.stringify({ port, token: "tk" }));
  };
  const nowhere = await mkdtemp(join(tmpdir(), "a12s-cwd-"));
  await assert.rejects(connect({ home, cwd: nowhere }), /no running Fleet found/);
  await write("proj", 52011);
  assert.equal((await connect({ home, cwd: nowhere })).url, "http://127.0.0.1:52011", "the only running Fleet");
  await write("other", 52012);
  await assert.rejects(connect({ home, cwd: nowhere }), /several Fleets are running \(other, proj\)/);
  assert.equal((await connect({ home, fleet: "other" })).url, "http://127.0.0.1:52012");

  const project = join(nowhere, "app", "src");
  await mkdir(project, { recursive: true });
  await writeFile(join(nowhere, "fleet.yaml"), "apiVersion: a12s.ai/v1alpha1\nkind: Fleet\nmetadata: { name: proj }\n");
  assert.equal((await connect({ home, cwd: project })).url, "http://127.0.0.1:52011", "nearest fleet.yaml upward");
  await assert.rejects(connect({ home, fleet: "nope" }), /Fleet nope isn't running/);
  assert.equal((await connect({ url: "http://h:1/", token: "x" })).url, "http://h:1");
});

test("fleetName reads flow and block metadata", () => {
  assert.equal(fleetName("metadata: { name: starter }\n"), "starter");
  assert.equal(fleetName("kind: Fleet\nmetadata:\n  labels: {}\n  name: \"lab\"\n"), "lab");
  assert.equal(fleetName("kind: Fleet\n"), undefined);
});

test("events: every Computer merged, ready() after each stream is live, then turns() folds them", async () => {
  const { client, close } = await fake((req, res) => {
    if (req.url === "/v1/status") {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ fleet: "f", computers: [{ name: "lead" }, { name: "w1" }, { name: "w2" }] }));
      return;
    }
    if (req.url === "/v1/computers/w2/events") {
      res.writeHead(503, { "content-type": "application/json" }).end(JSON.stringify({ code: "asleep", message: "asleep" }));
      return;
    }
    const name = req.url!.split("/")[3]!;
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.write(`event: head\ndata: {"seq":5}\n\n`);
    const e = (seq: number, type: string, data: unknown) =>
      `id: ${seq}\ndata: ${JSON.stringify({ v: 1, computer: name, session: "s", turn: `t_${name}`, seq, ts: "", type, data })}\n\n`;
    setTimeout(() => {
      res.write(e(6, "turn.started", { inbox: { batch: "b", pendingAfter: 0, messages: [{ id: "m", via: "local", sender: "app" }] } }));
      res.write(e(7, "text.delta", { text: `hi from ${name}` }));
      res.write(e(8, "tool.started", { id: "u", name: "mcp__a12s__send" }));
      res.write(e(9, "text.delta", { text: "SENT" }));
      res.write(e(10, "turn.finished", { status: "completed", usage: null }));
    }, 30);
  });
  try {
    const turns = client.turns(undefined, { reconnectMs: 50 });
    await turns.ready();
    const got: string[] = [];
    for await (const t of turns) {
      assert.equal(t.messages[0]!.via, "local");
      got.push(`${t.computer}:${t.status}:${t.text}`);
      if (got.length === 2) break;
    }
    assert.deepEqual(got.sort(), ["lead:completed:hi from lead\nSENT", "w1:completed:hi from w1\nSENT"]);
  } finally {
    close();
  }
});

test("events: a reconnect resumes from the head even when no event arrived", async () => {
  let calls = 0;
  const { client, close } = await fake((req, res) => {
    calls++;
    res.writeHead(200, { "content-type": "text/event-stream" });
    if (calls === 1) {
      res.write(`event: head\ndata: {"seq":41}\n\n`);
      setTimeout(() => res.destroy(), 20);
      return;
    }
    assert.equal(req.headers["last-event-id"], "41");
    res.write(ev(42, "diagnostic", "", { message: "after the drop" }));
  });
  try {
    const [e] = await collect(client.events("lead", { reconnectMs: 10 }), 1);
    assert.equal(e!.seq, 42);
  } finally {
    close();
  }
});

test("events: ready() rejects when a Computer doesn't exist", async () => {
  const { client, close } = await fake((_req, res) => {
    res.writeHead(404, { "content-type": "application/json" }).end(JSON.stringify({ code: "not_found", message: "no Computer x" }));
  });
  try {
    await assert.rejects(client.events("x").ready(), (e: unknown) => e instanceof A12sError && e.code === "not_found");
  } finally {
    close();
  }
});

test("events: turn and after need one Computer", () => {
  assert.throws(() => new A12sClient({ url: "http://x" }).events(["a", "b"], { after: 1 }), TypeError);
});
