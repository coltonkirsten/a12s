import assert from "node:assert/strict";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";

import { A12sError, Fleet, NotFound } from "../src/node.js";

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
      json(res, 200, { version, fleet: { apiVersion: "a", kind: "Fleet", metadata: { name: "f" }, agent_computers: {} }, drift: [] });
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

test("north star: create, prompt, remove", async () => {
  const ev = (seq: number, type: string, data: unknown) =>
    `id: ${seq}\ndata: ${JSON.stringify({ v: 1, computer: "hello", session: "s", turn: "t_1", seq, ts: "", type, data })}\n\n`;
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
    const turn = await hello.prompt("hi");
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
