import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { connect, createServer, type AddressInfo, type Socket } from 'node:net';
import { createServer as createHttpServer, request } from 'node:http';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import { parseUses, wire } from '../wiring.js';

/** A fake egress proxy: CONNECT svc.a12s:<port> and plain absolute-URI requests go to local servers. */
async function fakeProxy(routes: Record<string, number>): Promise<{ url: string; close: () => void; seen: string[] }> {
  const seen: string[] = [];
  const srv = createHttpServer((req, res) => {
    seen.push(`${req.method} ${req.url}`);
    const u = new URL(req.url ?? '');
    const port = routes[u.host];
    if (!port) {
      res.writeHead(403).end('denied');
      return;
    }
    const up = request({ host: '127.0.0.1', port, path: u.pathname + u.search, method: req.method, headers: req.headers }, (r) => {
      res.writeHead(r.statusCode ?? 502, r.headers);
      r.pipe(res);
    });
    req.pipe(up);
  });
  srv.on('connect', (req, sock: Socket, head) => {
    seen.push(`CONNECT ${req.url}`);
    const port = routes[req.url ?? ''];
    if (!port) {
      sock.end('HTTP/1.1 403 Forbidden\r\n\r\n');
      return;
    }
    const up = connect(port, '127.0.0.1', () => {
      sock.write('HTTP/1.1 200 Connection Established\r\n\r\n');
      if (head.length) up.write(head);
      up.pipe(sock);
      sock.pipe(up);
    });
  });
  await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r));
  return { url: `http://127.0.0.1:${(srv.address() as AddressInfo).port}`, close: () => srv.close(), seen };
}

test('a used TCP port gets a loopback forwarder bridged by CONNECT', async () => {
  const echo = createServer((s) => s.pipe(s));
  await new Promise<void>((r) => echo.listen(0, '127.0.0.1', r));
  const proxy = await fakeProxy({ 'pg.a12s:5432': (echo.address() as AddressInfo).port });
  const uses = parseUses(JSON.stringify([
    { service: 'pg', port: 'db', number: 5432, protocol: 'tcp', env: 'A12S_SERVICE_PG_DB' },
    { service: 'db', port: 'mcp', number: 8080, protocol: 'http', env: 'A12S_SERVICE_DB_MCP', mcpPath: '/mcp', tool: 'a12s-db' },
  ]));
  const w = await wire(uses, proxy.url, () => {});
  assert.equal(w.env.A12S_SERVICE_DB_MCP, 'http://db.a12s:8080');
  assert.match(w.env.A12S_SERVICE_PG_DB ?? '', /^127\.0\.0\.1:\d+$/);
  assert.deepEqual(w.mcpServers['a12s-db']?.args.slice(1), ['http://db.a12s:8080/mcp']);
  const [, port] = (w.env.A12S_SERVICE_PG_DB ?? '').split(':');
  const got = await new Promise<string>((resolve) => {
    const c = connect(Number(port), '127.0.0.1', () => c.write('ping'));
    c.on('data', (d) => {
      resolve(d.toString());
      c.destroy();
    });
  });
  assert.equal(got, 'ping');
  assert.ok(proxy.seen.includes('CONNECT pg.a12s:5432'));
  w.servers.forEach((s) => s.close());
  echo.close();
  proxy.close();
});

test('the stdio bridge carries MCP JSON-RPC to an HTTP endpoint through the proxy', async () => {
  let session = '';
  const mcp = createHttpServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      const m = JSON.parse(body) as { id?: number; method: string };
      if (m.method === 'initialize') {
        session = 's-1';
        res.writeHead(200, { 'content-type': 'application/json', 'mcp-session-id': session });
        res.end(JSON.stringify({ jsonrpc: '2.0', id: m.id, result: { protocolVersion: '2025-06-18' } }));
        return;
      }
      assert.equal(req.headers['mcp-session-id'], 's-1');
      if (m.id === undefined) {
        res.writeHead(202).end();
        return;
      }
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      res.end(`event: message\ndata: ${JSON.stringify({ jsonrpc: '2.0', id: m.id, result: { tools: [{ name: 'query' }] } })}\n\n`);
    });
  });
  await new Promise<void>((r) => mcp.listen(0, '127.0.0.1', r));
  const proxy = await fakeProxy({ 'db.a12s:8080': (mcp.address() as AddressInfo).port });
  const bridge = join(dirname(fileURLToPath(import.meta.url)), '..', 'mcpbridge.js');
  const child = spawn(process.execPath, [bridge, 'http://db.a12s:8080/mcp'], { env: { ...process.env, A12S_PROXY: proxy.url } });
  const lines: string[] = [];
  child.stdout.setEncoding('utf8').on('data', (d: string) => lines.push(...d.trim().split('\n')));
  child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {} }) + '\n');
  child.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
  child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 2, method: 'tools/list' }) + '\n');
  for (let i = 0; i < 100 && lines.length < 2; i++) await new Promise((r) => setTimeout(r, 20));
  child.stdin.end();
  assert.equal(lines.length, 2, lines.join('\n'));
  assert.equal(JSON.parse(lines[0] ?? '{}').id, 1);
  assert.equal(JSON.parse(lines[1] ?? '{}').result.tools[0].name, 'query');
  assert.ok(proxy.seen.includes('POST http://db.a12s:8080/mcp'));
  child.kill();
  mcp.close();
  proxy.close();
});
