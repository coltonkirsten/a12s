// Service wiring inside a Computer (enhancements §5.4): every port of a used
// Service gets an address variable; a TCP port gets a local forwarder on an
// allocated loopback port bridged through the egress proxy with CONNECT; an
// MCP port the Computer wants as a tool is registered with the harness through
// the stdio bridge (dist/mcpbridge.js). The proxy still makes every decision.
import { connect, createServer, type Server, type Socket } from 'node:net';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { McpServer } from './adapters/adapter.js';

export interface Endpoint {
  service: string;
  port: string;
  number: number;
  protocol: 'http' | 'tcp';
  env: string;
  mcpPath?: string;
  tool?: string;
}

export function parseUses(raw: string | undefined): Endpoint[] {
  if (!raw) return [];
  const v = JSON.parse(raw) as unknown;
  if (!Array.isArray(v)) throw new Error('A12S_USES must be a JSON list');
  return v.map((e: Record<string, unknown>) => {
    if (typeof e.service !== 'string' || typeof e.number !== 'number' || typeof e.env !== 'string' || !/^A12S_[A-Z0-9_]+$/.test(e.env)) {
      throw new Error(`A12S_USES: bad entry ${JSON.stringify(e)}`);
    }
    return {
      service: e.service,
      port: String(e.port ?? ''),
      number: e.number,
      protocol: e.protocol === 'tcp' ? 'tcp' : 'http',
      env: e.env,
      mcpPath: typeof e.mcpPath === 'string' ? e.mcpPath : undefined,
      tool: typeof e.tool === 'string' ? e.tool : undefined,
    };
  });
}

/** Opens a CONNECT tunnel to target (host:port) through the HTTP proxy. */
export function dialThroughProxy(proxy: string, target: string): Promise<Socket> {
  const u = new URL(proxy);
  return new Promise((resolve, reject) => {
    const s = connect({ host: u.hostname, port: Number(u.port || 80) });
    s.setNoDelay(true);
    let head = Buffer.alloc(0);
    const onData = (d: Buffer) => {
      head = Buffer.concat([head, d]);
      const end = head.indexOf('\r\n\r\n');
      if (end < 0) {
        if (head.length > 16384) fail(new Error('proxy answer too long'));
        return;
      }
      s.off('data', onData);
      const status = head.subarray(0, head.indexOf('\r\n')).toString();
      if (!/^HTTP\/1\.[01] 200/.test(status)) return fail(new Error(`proxy refused ${target}: ${head.subarray(0, Math.min(head.length, 400)).toString().split('\r\n')[0]}`));
      const rest = head.subarray(end + 4);
      if (rest.length) s.unshift(rest);
      resolve(s);
    };
    const fail = (err: Error) => {
      s.destroy();
      reject(err);
    };
    s.on('data', onData);
    s.once('error', fail);
    s.write(`CONNECT ${target} HTTP/1.1\r\nHost: ${target}\r\n\r\n`);
  });
}

export interface Wiring {
  env: Record<string, string>;
  mcpServers: Record<string, McpServer>;
  servers: Server[];
}

/** Starts the forwarders and returns the env and MCP servers to give the harness. */
export async function wire(uses: Endpoint[], proxy: string | null, log: (m: string) => void): Promise<Wiring> {
  const out: Wiring = { env: {}, mcpServers: {}, servers: [] };
  const bridge = join(dirname(fileURLToPath(import.meta.url)), 'mcpbridge.js');
  for (const e of uses) {
    const base = `http://${e.service}.a12s:${e.number}`;
    if (e.protocol === 'http') {
      out.env[e.env] = base;
      if (e.tool && e.mcpPath) {
        out.mcpServers[e.tool] = { command: process.execPath, args: [bridge, base + e.mcpPath], env: proxy ? { A12S_PROXY: proxy } : {} };
      }
      continue;
    }
    if (!proxy) {
      log(`no proxy: ${e.service}:${e.number} is not wired`);
      continue;
    }
    const target = `${e.service}.a12s:${e.number}`;
    const srv = createServer((c) => {
      dialThroughProxy(proxy, target).then(
        (u) => {
          c.pipe(u);
          u.pipe(c);
          const close = () => {
            c.destroy();
            u.destroy();
          };
          c.once('close', close);
          u.once('close', close);
          c.on('error', close);
          u.on('error', close);
        },
        (err: Error) => {
          log(`forwarder ${target}: ${err.message}`);
          c.destroy();
        },
      );
    });
    await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r));
    const addr = srv.address();
    if (addr && typeof addr === 'object') out.env[e.env] = `127.0.0.1:${addr.port}`;
    out.servers.push(srv);
    log(`forwarder ${out.env[e.env]} -> ${target}`);
  }
  return out;
}
