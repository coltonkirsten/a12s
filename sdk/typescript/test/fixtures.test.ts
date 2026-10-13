import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";

import { Client } from "../src/client.js";
import { A12sError, Fleet } from "../src/index.js";
import * as fx from "./api-fixtures.gen.js";

// The fixtures are rendered from the Go API types (internal/api/sdkfixtures_test.go); tsc checks them against
// src/types.ts. Here the client parses each one off the wire.
const routes: Record<string, unknown> = {
  "GET /v1/status": fx.statusResponse,
  "POST /v1/agent-computers/lead/inbox": fx.enqueueResponse,
  "GET /v1/agent-computers/lead/inbox": fx.inboxListResponse,
  "GET /v1/agent-computers/lead/inbox/m_1": fx.message,
  "GET /v1/services": fx.servicesResponse,
  "GET /v1/services/svc": fx.servicesResponse.services[0],
  "POST /v1/tokens": fx.token,
  "GET /v1/fleet": fx.fleetResponse,
  "GET /v1/agent-computers": fx.agentComputersResponse,
  "PUT /v1/agent-computers/name": fx.putFleetResponse,
};

test("the client parses every Go-rendered API fixture", async () => {
  const srv = createServer((req, res) => {
    const body = routes[`${req.method} ${req.url}`];
    if (body === undefined) {
      res.writeHead(403, { "content-type": "application/json" }).end(JSON.stringify(fx.error));
      return;
    }
    res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify(body));
  });
  await new Promise<void>((r) => srv.listen(0, "127.0.0.1", r));
  const url = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
  const client = new Client({ url, token: "t" });
  const fleet = new Fleet({ url, token: "t" });
  try {
    assert.deepEqual(await client.status(), fx.statusResponse);
    assert.deepEqual(await client.send("lead", fx.enqueueRequest), fx.enqueueResponse);
    assert.deepEqual(await client.inbox("lead"), fx.inboxListResponse);
    assert.deepEqual(await client.message("lead", "m_1"), fx.message);
    assert.deepEqual(await client.services(), fx.servicesResponse.services);
    assert.deepEqual(await client.service("svc"), fx.servicesResponse.services[0]);
    assert.deepEqual(await client.createToken(fx.tokenRequest), fx.token);
    assert.deepEqual((await fleet.get()).spec, fx.fleetResponse.fleet);
    const [ac] = await fleet.agentComputers.list();
    assert.deepEqual(ac!.status, fx.agentComputersResponse.agentComputers[0]!.status);
    assert.deepEqual((await fleet.agentComputers.create("name")).status, fx.putFleetResponse.agentComputer!.status);
    await assert.rejects(client.withdraw("lead", "m_1"), (e: unknown) => {
      assert.ok(e instanceof A12sError);
      assert.equal(e.code, fx.error.code);
      assert.deepEqual(e.details, fx.error.details);
      return true;
    });
  } finally {
    srv.close();
  }
});
