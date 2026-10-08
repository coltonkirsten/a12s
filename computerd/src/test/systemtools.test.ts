import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js';
import type { Computer } from '../computer.js';
import { localHandler } from '../server.js';
import { testConfig } from './helpers.js';

test('system tools MCP server forwards to the computerd socket', async () => {
  const cfg = testConfig();
  mkdirSync(dirname(cfg.sockPath), { recursive: true });
  let restarts = 0;
  const computer = { requestRestart: () => `restart #${++restarts}` } as unknown as Computer;
  const server = createServer(localHandler(computer, null));
  await new Promise<void>((r) => server.listen(cfg.sockPath, r));
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [join(dirname(fileURLToPath(import.meta.url)), '..', 'systemtools.js')],
    env: { A12S_SOCK: cfg.sockPath },
  });
  const client = new Client({ name: 'test', version: '0' });
  try {
    await client.connect(transport);
    const { tools } = await client.listTools();
    assert.deepEqual(tools.map((t) => t.name), ['restart_harness', 'lock', 'release']);
    const r = await client.callTool({ name: 'restart_harness', arguments: {} });
    assert.equal(restarts, 1);
    assert.deepEqual(r.content, [{ type: 'text', text: '{"message":"restart #1"}' }]);
    const lock = await client.callTool({ name: 'lock', arguments: { drive: 'team', path: 'a.txt' } });
    assert.equal(lock.isError, true, 'no controller TLS here, so the lease call is refused');
  } finally {
    await client.close();
    server.close();
  }
});

test('the send tool exists only with a grant and returns the Controller reply verbatim', async () => {
  const cfg = testConfig();
  mkdirSync(dirname(cfg.sockPath), { recursive: true });
  const sent: unknown[] = [];
  const computer = {} as unknown as Computer;
  const server = createServer(
    localHandler(computer, null, async (req) => {
      sent.push(req);
      if (req.computer !== 'reviewer') return { status: 403, body: { code: 'no_grant', message: `builder may not send to ${req.computer}`, fix: `add ${req.computer} to computers.builder.grants.send` } };
      return { status: 201, body: { id: 'm_1', state: 'pending' } };
    }),
  );
  await new Promise<void>((r) => server.listen(cfg.sockPath, r));
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [join(dirname(fileURLToPath(import.meta.url)), '..', 'systemtools.js')],
    env: { A12S_SOCK: cfg.sockPath, A12S_SEND: 'reviewer' },
  });
  const client = new Client({ name: 'test', version: '0' });
  try {
    await client.connect(transport);
    const { tools } = await client.listTools();
    const send = tools.find((t) => t.name === 'send');
    assert.ok(send && send.description?.includes('You may send to: reviewer'));
    const ok = await client.callTool({ name: 'send', arguments: { computer: 'reviewer', body: 'please review', replaceKey: 'r1' } });
    assert.equal(ok.isError, false);
    const denied = await client.callTool({ name: 'send', arguments: { computer: 'scout', body: 'hi' } });
    assert.equal(denied.isError, true);
    assert.match(JSON.stringify(denied.content), /add scout to computers.builder.grants.send/);
    assert.deepEqual(sent[0], { computer: 'reviewer', body: 'please review', replaceKey: 'r1' });
  } finally {
    await client.close();
    server.close();
  }
});
