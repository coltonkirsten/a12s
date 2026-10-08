#!/usr/bin/env node
// svc-mcp: the reference MCP wrapper Service (enhancements §5.8). It serves any
// stdio MCP server over Streamable HTTP at :8080/mcp, one child process per MCP
// session, with credentials mapped into the child's env (envFrom) and forwarder
// addresses split into host/port variables (hostPortFrom). Computers that
// `uses:` it get the tool through computerd's stdio bridge; they never see the
// credential. No dependencies: plain Node.
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { createInterface } from 'node:readline';

const CONFIG = process.env.A12S_CONFIG ?? '/run/a12s/config/config.json';
const SECRETS = process.env.A12S_SECRETS ?? '/run/a12s/secrets';
const PORT = Number(process.env.SVC_MCP_PORT ?? 8080);
const IDLE_MS = 30 * 60 * 1000;
const MAX_BODY = 4 << 20;

const log = (m) => process.stdout.write(`svc-mcp: ${m}\n`);
const cfg = JSON.parse(readFileSync(CONFIG, 'utf8'));
const command = Array.isArray(cfg.command) ? cfg.command : cfg.command ? [cfg.command] : [];
if (!command.length) {
  log('config.command is required (the stdio MCP server to run)');
  process.exit(1);
}

function childEnv() {
  const env = { ...process.env, ...(cfg.env ?? {}) };
  for (const [name, cred] of Object.entries(cfg.envFrom ?? {})) {
    if (!/^[a-z][a-z0-9_-]*$/.test(cred)) throw new Error(`envFrom ${name}: bad credential name ${cred}`);
    env[name] = readFileSync(`${SECRETS}/${cred}`, 'utf8').trim();
  }
  for (const [src, [hostVar, portVar]] of Object.entries(cfg.hostPortFrom ?? {})) {
    const addr = process.env[src];
    if (!addr) throw new Error(`hostPortFrom: ${src} is not set (is the Service in uses?)`);
    const i = addr.lastIndexOf(':');
    env[hostVar] = addr.slice(0, i);
    env[portVar] = addr.slice(i + 1);
  }
  return env;
}

const expand = (s, env) => String(s).replace(/\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (_, k) => env[k] ?? '');

const sessions = new Map(); // id -> { child, pending: Map<id, resolve>, timer }

function startSession() {
  const env = childEnv();
  const argv = [...command, ...(cfg.args ?? [])].map((a) => expand(a, env));
  const child = spawn(argv[0], argv.slice(1), { env, stdio: ['pipe', 'pipe', 'pipe'] });
  const id = randomBytes(16).toString('hex');
  const s = { child, pending: new Map(), timer: null };
  child.stderr.setEncoding('utf8').on('data', (d) => process.stdout.write(`[${id.slice(0, 6)}] ${d}`));
  createInterface({ input: child.stdout }).on('line', (line) => {
    let m;
    try {
      m = JSON.parse(line);
    } catch {
      return;
    }
    const key = m.id !== undefined ? JSON.stringify(m.id) : null;
    const resolve = key !== null && (m.result !== undefined || m.error !== undefined) ? s.pending.get(key) : undefined;
    if (resolve) {
      s.pending.delete(key);
      resolve(m);
    }
  });
  child.on('exit', (code) => {
    for (const [key, resolve] of s.pending) resolve({ jsonrpc: '2.0', id: JSON.parse(key), error: { code: -32000, message: `MCP server exited (${code})` } });
    s.pending.clear();
    sessions.delete(id);
    clearTimeout(s.timer);
    log(`session ${id.slice(0, 6)} ended (exit ${code})`);
  });
  sessions.set(id, s);
  touch(id);
  log(`session ${id.slice(0, 6)} started: ${argv[0]}`);
  return id;
}

function touch(id) {
  const s = sessions.get(id);
  if (!s) return;
  clearTimeout(s.timer);
  s.timer = setTimeout(() => s.child.kill('SIGTERM'), IDLE_MS);
  s.timer.unref();
}

function send(res, status, body, headers = {}) {
  res.writeHead(status, { 'content-type': 'application/json', ...headers });
  res.end(body === undefined ? '' : JSON.stringify(body));
}

createServer((req, res) => {
  const url = new URL(req.url, 'http://x');
  if (req.method === 'GET' && url.pathname === '/healthz') return send(res, 200, { ok: true });
  if (url.pathname !== (cfg.path ?? '/mcp')) return send(res, 404, { error: 'not found' });
  const sid = req.headers['mcp-session-id'];
  if (req.method === 'DELETE') {
    sessions.get(sid)?.child.kill('SIGTERM');
    return send(res, 204);
  }
  if (req.method !== 'POST') return send(res, 405, { error: 'POST only' });
  let body = '';
  req.setEncoding('utf8');
  req.on('data', (d) => {
    body += d;
    if (body.length > MAX_BODY) req.destroy();
  });
  req.on('end', async () => {
    let msgs;
    try {
      const parsed = JSON.parse(body);
      msgs = Array.isArray(parsed) ? parsed : [parsed];
    } catch {
      return send(res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'parse error' } });
    }
    let id = typeof sid === 'string' ? sid : null;
    if (!id) {
      if (!msgs.some((m) => m.method === 'initialize')) return send(res, 400, { error: 'no session: initialize first' });
      try {
        id = startSession();
      } catch (err) {
        log(`cannot start the MCP server: ${err.message}`);
        return send(res, 500, { jsonrpc: '2.0', id: msgs[0].id ?? null, error: { code: -32000, message: err.message } });
      }
    }
    const s = sessions.get(id);
    if (!s) return send(res, 404, { error: 'unknown or ended session' });
    touch(id);
    const waits = [];
    for (const m of msgs) {
      if (m.method !== undefined && m.id !== undefined) waits.push(new Promise((resolve) => s.pending.set(JSON.stringify(m.id), resolve)));
      s.child.stdin.write(JSON.stringify(m) + '\n');
    }
    if (!waits.length) return send(res, 202, undefined, { 'mcp-session-id': id });
    const answers = await Promise.all(waits);
    send(res, 200, Array.isArray(JSON.parse(body)) ? answers : answers[0], { 'mcp-session-id': id });
  });
}).listen(PORT, () => log(`serving ${command[0]} over HTTP on :${PORT}${cfg.path ?? '/mcp'}`));
