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
