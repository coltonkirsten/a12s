#!/usr/bin/env node
// a12s stdio -> HTTP MCP bridge. The harness speaks MCP over
// stdio; a Service serves it over Streamable HTTP at <url>. Each JSON-RPC line from
// stdin is POSTed (through A12S_PROXY as a plain-HTTP absolute-URI request, the one
// form the proxy accepts for .a12s names); JSON and SSE answers go to stdout.
import { request } from 'node:http';
import { createInterface } from 'node:readline';

const target = new URL(process.argv[2] ?? '');
const proxy = process.env.A12S_PROXY ? new URL(process.env.A12S_PROXY) : null;
let session: string | null = null;
let queue = Promise.resolve();

function out(msg: string): void {
  if (msg.trim()) process.stdout.write(msg.trim() + '\n');
}

function post(body: string): Promise<void> {
  return new Promise((resolve) => {
    const headers: Record<string, string | number> = {
      'content-type': 'application/json',
      accept: 'application/json, text/event-stream',
      'content-length': Buffer.byteLength(body),
      host: target.host,
    };
    if (session) headers['mcp-session-id'] = session;
    const opts = proxy
      ? { host: proxy.hostname, port: Number(proxy.port || 80), path: target.href, method: 'POST', headers }
      : { host: target.hostname, port: Number(target.port || 80), path: target.pathname + target.search, method: 'POST', headers };
    const req = request(opts, (res) => {
      const sid = res.headers['mcp-session-id'];
      if (typeof sid === 'string') session = sid;
      const sse = String(res.headers['content-type'] ?? '').includes('text/event-stream');
      let buf = '';
      res.setEncoding('utf8');
      res.on('data', (d: string) => {
        buf += d;
        if (!sse) return;
        let i: number;
        while ((i = buf.indexOf('\n\n')) >= 0) {
          const event = buf.slice(0, i);
          buf = buf.slice(i + 2);
          out(event.split('\n').filter((l) => l.startsWith('data:')).map((l) => l.slice(5).trimStart()).join('\n'));
        }
      });
      res.on('end', () => {
        if (!sse && res.statusCode !== 202) {
          if ((res.statusCode ?? 500) >= 400) errorReply(body, `HTTP ${res.statusCode}: ${buf.slice(0, 500)}`);
          else out(buf);
        }
        resolve();
      });
    });
    req.on('error', (err) => {
      errorReply(body, err.message);
      resolve();
    });
    req.end(body);
  });
}

/** Answers a request the Service could not take, so the harness is not left waiting. */
function errorReply(body: string, message: string): void {
  try {
    const m = JSON.parse(body) as { id?: unknown };
    if (m.id !== undefined) out(JSON.stringify({ jsonrpc: '2.0', id: m.id, error: { code: -32000, message: `a12s bridge: ${message}` } }));
  } catch {
    // not JSON: nothing to answer
  }
}

createInterface({ input: process.stdin }).on('line', (line) => {
  if (!line.trim()) return;
  // In order: the first request carries initialize and sets the session.
  queue = queue.then(() => post(line));
});
process.stdin.on('end', () => void queue.then(() => process.exit(0)));
