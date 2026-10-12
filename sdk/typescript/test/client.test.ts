import assert from "node:assert/strict";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";

import { A12sClient, A12sError } from "../src/index.js";

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
    assert.deepEqual([r.turn, r.status, r.text], ["t_9", "completed", "hello"]);
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
