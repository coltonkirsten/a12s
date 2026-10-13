import assert from "node:assert/strict";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";

import { A12sError, Fleet, NotFound } from "../src/node.js";
import * as fx from "./api-fixtures.gen.js";

type Handler = (req: IncomingMessage, res: ServerResponse, body: string) => void;
type Seen = { method: string; url: string; headers: IncomingMessage["headers"]; body: string };

async function fake(handler: Handler): Promise<{ fleet: Fleet; seen: Seen[]; close: () => void }> {
  const seen: Seen[] = [];
  const srv = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      seen.push({ method: req.method!, url: req.url!, headers: req.headers, body });
      handler(req, res, body);
    });
  });
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
  const { port } = srv.address() as AddressInfo;
  return {
    fleet: await Fleet.connect({ url: `http://127.0.0.1:${port}`, token: "tok" }),
    seen,
    close: () => {
      srv.closeAllConnections();
      srv.close();
    },
  };
}

const json = (res: ServerResponse, status: number, body: unknown) =>
  res.writeHead(status, { "content-type": "application/json" }).end(JSON.stringify(body));

const view = (name: string, spec: object = {}) => ({ name, spec, effective: spec, status: { name, phase: "Ready" } });
const written = (name: string, spec: object = {}) => ({ version: 2, changes: [], agentComputer: view(name, spec) });

test("create sends If-None-Match: * unless replace, and returns the written agent computer", async () => {
  const { fleet, seen, close } = await fake((_req, res, body) => json(res, 200, written("hello", JSON.parse(body))));
  try {
    const hello = await fleet.agentComputers.create("hello", { instructions: "Be helpful" });
    assert.deepEqual([hello.name, hello.spec, hello.status.phase], ["hello", { instructions: "Be helpful" }, "Ready"]);
    await fleet.agentComputers.create("hello", { instructions: "x", replace: true });
    assert.deepEqual(seen.map((s) => [s.method, s.url, s.headers["if-none-match"], s.body]), [
      ["PUT", "/v1/agent-computers/hello", "*", '{"instructions":"Be helpful"}'],
      ["PUT", "/v1/agent-computers/hello", undefined, '{"instructions":"x"}'],
    ]);
  } finally {
    close();
  }
});

test("create on a taken name surfaces code exists; get on a missing one throws NotFound", async () => {
  const { fleet, close } = await fake((req, res) =>
    req.method === "PUT"
      ? json(res, 409, { code: "exists", message: "agent computer hello already exists" })
      : json(res, 404, { code: "not_found", message: 'no agent computer "nope" in the Fleet' }),
  );
  try {
    await assert.rejects(fleet.agentComputers.create("hello"), (e: unknown) => {
      assert.ok(e instanceof A12sError && !(e instanceof NotFound));
      assert.equal(e.code, "exists");
      return true;
    });
    await assert.rejects(fleet.agentComputers.get("nope"), (e: unknown) => e instanceof NotFound && e.code === "not_found");
  } finally {
    close();
  }
});

test("update sends the merge patch and refreshes the handle", async () => {
  const { fleet, seen, close } = await fake((req, res, body) =>
    req.method === "GET" ? json(res, 200, view("hello", { image: "slim" })) : json(res, 200, written("hello", { power: JSON.parse(body).power })),
  );
  try {
    const hello = await fleet.agentComputers.get("hello");
    await hello.update({ power: { sleepAfterIdle: "15m" }, image: null });
    assert.deepEqual(seen[1], {
      ...seen[1]!,
      method: "PATCH",
      url: "/v1/agent-computers/hello",
      body: '{"power":{"sleepAfterIdle":"15m"},"image":null}',
    });
    assert.deepEqual(hello.spec, { power: { sleepAfterIdle: "15m" } });
  } finally {
    close();
  }
});

test("replace retries a stale If-Match, then writes", async () => {
  let version = 6;
  const { fleet, seen, close } = await fake((req, res, body) => {
    if (req.method === "GET") {
      json(res, 200, { version, fleet: { apiVersion: "a", kind: "Fleet", metadata: { name: "f" }, agent_computers: {} } });
      return;
    }
    if (req.headers["if-match"] === '"6"') {
      version = 7;
      json(res, 412, { code: "version_mismatch", message: "the Fleet changed" });
      return;
    }
    assert.deepEqual(JSON.parse(body).fleet.defaults, { power: { sleepAfterIdle: "15m" } });
    json(res, 200, { version: ++version, changes: [] });
  });
  try {
    const state = await fleet.replace((f) => {
      f.defaults!.power = { sleepAfterIdle: "15m" };
    });
    assert.equal(state.version, 8);
    assert.deepEqual(seen.map((s) => `${s.method} ${s.headers["if-match"] ?? ""}`), ["GET ", "PUT \"6\"", "GET ", "PUT \"7\"", "GET "]);
  } finally {
    close();
  }
});

test("north star: create, send(...).wait(), remove", async () => {
  const ev = (seq: number, type: string, data: unknown) =>
    `id: ${seq}\ndata: ${JSON.stringify({ v: 1, agentComputer: "hello", session: "s", turn: "t_1", seq, ts: "", type, data })}\n\n`;
  const { fleet, seen, close } = await fake((req, res) => {
    if (req.method === "PUT") return json(res, 200, written("hello"));
    if (req.method === "POST") return json(res, 201, { id: "m_1", seq: 1, state: "pending" });
    if (req.method === "DELETE") return json(res, 200, { version: 3, changes: [{ op: "remove", kind: "agentComputer", name: "hello", when: "now" }] });
    res.writeHead(200, { "content-type": "text/event-stream" });
    if (req.url!.includes("/inbox/")) return res.end(`data: ${JSON.stringify({ type: "delivered", data: { turn: "t_1" } })}\n\n`);
    res.write(ev(1, "text.delta", { text: "hi there" }));
    res.end(ev(2, "turn.finished", { status: "completed", usage: null }));
  });
  try {
    const hello = await fleet.agentComputers.create("hello", { instructions: "Be helpful" });
    const turn = await hello.send("hi").wait();
    assert.equal(turn.text, "hi there");
    await hello.remove();
    assert.deepEqual(seen.map((s) => `${s.method} ${s.url}`), [
      "PUT /v1/agent-computers/hello",
      "POST /v1/agent-computers/hello/inbox",
      "GET /v1/agent-computers/hello/inbox/m_1/events",
      "GET /v1/agent-computers/hello/events?turn=t_1",
      "DELETE /v1/agent-computers/hello",
    ]);
    assert.ok(seen.every((s) => s.headers.authorization === "Bearer tok"));
  } finally {
    close();
  }
});

test("wait() after the Turn already ended replays it from the Message's own snapshot", async () => {
  const ev = (seq: number, type: string, data: unknown) =>
    `id: ${seq}\ndata: ${JSON.stringify({ v: 1, agentComputer: "hello", session: "s", turn: "t_1", seq, ts: "", type, data })}\n\n`;
  let finished = false;
  const { fleet, seen, close } = await fake((req, res) => {
    if (req.method === "GET" && req.url === "/v1/agent-computers/hello") return json(res, 200, view("hello"));
    if (req.method === "POST") {
      finished = true;
      return json(res, 201, { id: "m_1", seq: 1, state: "pending" });
    }
    assert.ok(finished, "nothing is watched before the Message is sent");
    res.writeHead(200, { "content-type": "text/event-stream" });
    if (req.url!.includes("/inbox/")) {
      return res.end(`data: ${JSON.stringify({ type: "snapshot", data: { message: { id: "m_1", state: "delivered", turn: "t_1" } } })}\n\n`);
    }
    res.write(ev(1, "text.delta", { text: "out of credit" }));
    res.end(ev(2, "turn.finished", { status: "failed", error: "billing", usage: null }));
  });
  try {
    const hello = await fleet.agentComputers.get("hello");
    const m = await hello.send("hi", { fresh: true });
    assert.deepEqual([m.id, m.state], ["m_1", "pending"]);
    const seenTypes: string[] = [];
    const r = await m.wait({ onEvent: (e) => seenTypes.push(e.type) });
    assert.deepEqual([r.id, r.status, r.text, r.message.id], ["t_1", "failed", "out of credit", "m_1"]);
    assert.deepEqual(seenTypes, ["text.delta", "turn.finished"]);
    assert.deepEqual(seen.slice(1).map((s) => `${s.method} ${s.url}`), [
      "POST /v1/agent-computers/hello/inbox",
      "GET /v1/agent-computers/hello/inbox/m_1/events",
      "GET /v1/agent-computers/hello/events?turn=t_1",
    ]);
    assert.equal(JSON.parse(seen[1]!.body).fresh, true);
  } finally {
    close();
  }
});

test("wait() on a held inbox throws A12sError held", async () => {
  const { fleet, close } = await fake((req, res) => {
    if (req.method === "GET" && req.url === "/v1/agent-computers/hello") return json(res, 200, view("hello"));
    if (req.method === "POST") return json(res, 201, { id: "m_1", seq: 1, state: "pending" });
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.end(`data: ${JSON.stringify({ type: "snapshot", data: { message: { id: "m_1", state: "pending" }, held: true, holdReason: "paused" } })}\n\n`);
  });
  try {
    const hello = await fleet.agentComputers.get("hello");
    await assert.rejects(hello.send("hi").wait(), (e: unknown) => e instanceof A12sError && e.code === "held");
  } finally {
    close();
  }
});

const fev = (id: string, agentComputer: string, type: string, data: unknown, turn = "") =>
  `id: ${id}\ndata: ${JSON.stringify({ v: 1, agentComputer, session: "s", turn, seq: 1, ts: "", type, data })}\n\n`;

async function take<T>(it: AsyncIterator<T>, n: number): Promise<T[]> {
  const out: T[] = [];
  while (out.length < n) out.push((await it.next()).value as T);
  return out;
}

test("agent computers' and the Fleet's streams share one /v1/events connection, each filtered", async () => {
  const conns: ServerResponse[] = [];
  const { fleet, seen, close } = await fake((req, res) => {
    if (req.url !== "/v1/events") return json(res, 200, view(req.url!.split("/").pop()!));
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.write('event: head\ndata: {"id":"b-0"}\n\n');
    conns.push(res);
  });
  try {
    const [a, b] = await Promise.all([fleet.agentComputers.get("a"), fleet.agentComputers.get("b")]);
    const ea = a.events();
    const eb = b.events();
    const all = fleet.events();
    const turns = fleet.turns(["b"]);
    await Promise.all([ea.ready(), eb.ready(), all.ready(), turns.ready()]);
    conns[0]!.write(fev("b-1", "a", "text.delta", { text: "from a" }, "t_a"));
    conns[0]!.write(fev("b-2", "b", "egress.denied", { host: "evil.com", port: 443 }));
    conns[0]!.write(fev("b-3", "", "fleet.changed", { version: 2, by: "owner", names: ["a"] }));
    conns[0]!.write(
      fev("b-4", "b", "turn.finished", { status: "completed", usage: null, tokens: { input: 1, output: 2, cacheRead: 0, cacheWrite: 0 } }, "t_b"),
    );
    assert.deepEqual((await take(ea, 2)).map((e) => e.type), ["text.delta", "fleet.changed"]);
    assert.deepEqual((await take(eb, 2)).map((e) => e.type), ["egress.denied", "turn.finished"]);
    assert.deepEqual((await take(all, 4)).map((e) => e.type), ["text.delta", "egress.denied", "fleet.changed", "turn.finished"]);
    const [turn] = await take(turns, 1);
    assert.deepEqual([turn!.agentComputer, turn!.id, turn!.tokens?.output], ["b", "t_b", 2]);
    for (const s of [ea, eb, all, turns]) s.close();
    assert.equal(seen.filter((s) => s.url === "/v1/events").length, 1);
  } finally {
    close();
  }
});

test("the shared stream reconnects with Last-Event-ID", async () => {
  const { fleet, seen, close } = await fake((req, res) => {
    res.writeHead(200, { "content-type": "text/event-stream" });
    if (req.headers["last-event-id"] === undefined) {
      res.write('event: head\ndata: {"id":"b-0"}\n\n');
      res.end(fev("b-1", "a", "text.delta", { text: "one" }));
      return;
    }
    res.write(fev("b-2", "a", "text.delta", { text: "two" }));
  });
  try {
    const events = fleet.events("a", { reconnectMs: 10 });
    const got = await take(events, 2);
    events.close();
    assert.deepEqual(got.map((e) => (e.type === "text.delta" ? e.data.text : "")), ["one", "two"]);
    assert.deepEqual(seen.map((s) => s.headers["last-event-id"]), [undefined, "b-1"]);
  } finally {
    close();
  }
});

test("turns({ limit }) reads past Turns", async () => {
  const { fleet, seen, close } = await fake((req, res) =>
    req.url!.includes("/turns") ? json(res, 200, fx.turnsResponseMinimal) : json(res, 200, view("a")),
  );
  try {
    const a = await fleet.agentComputers.get("a");
    const [turn] = await a.turns({ limit: 5 });
    assert.equal(seen[1]!.url, "/v1/agent-computers/a/turns?limit=5");
    assert.deepEqual([turn!.agentComputer, turn!.tools[0]!.isError], ["agentComputer", false]);
  } finally {
    close();
  }
});
