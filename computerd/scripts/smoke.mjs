#!/usr/bin/env node
// Real-harness smoke run in dev mode: temp home/run dirs, a throwaway CA, the local harness binary as the bundle.
// Usage: node scripts/smoke.mjs claude-code|codex   (env: CLAUDE_BIN, CODEX_BIN, A12S_LAB_CREDS)
import { execFileSync, spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { request } from 'node:https';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const kind = process.argv[2];
if (kind !== 'claude-code' && kind !== 'codex') throw new Error('usage: smoke.mjs claude-code|codex');
const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const lab = process.env.A12S_LAB_CREDS ?? join(process.env.HOME, '.a12s/lab');
const T = mkdtempSync(join(tmpdir(), 'a12s-smoke-'));
const run = join(T, 'run');
const ca = join(T, 'ca');
for (const d of ['home', 'bundle/bin', 'run/secrets', 'run/tls', 'ca']) mkdirSync(join(T, d), { recursive: true });
symlinkSync(process.env.CLAUDE_BIN ?? join(process.env.HOME, '.local/bin/claude'), join(T, 'bundle/bin/claude'));
symlinkSync(process.env.CODEX_BIN ?? '/opt/homebrew/bin/codex', join(T, 'bundle/bin/codex'));
const cred = kind === 'codex' ? 'openai' : 'anthropic';
copyFileSync(join(lab, `${cred}.json`), join(run, 'secrets', cred)); // never printed
chmodSync(join(run, "secrets", cred), 0o600);

const ssl = (...args) => execFileSync('openssl', args, { stdio: ['ignore', 'ignore', 'pipe'] });
const ec = ['-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256', '-nodes'];
ssl('req', '-x509', ...ec, '-keyout', join(ca, 'ca.key'), '-out', join(ca, 'ca.crt'), '-subj', '/CN=a12s-smoke-ca', '-days', '1');
function issue(name, ext, keyOut, certOut) {
  writeFileSync(join(ca, `${name}.ext`), ext);
  ssl('req', ...ec, '-keyout', keyOut, '-out', join(ca, `${name}.csr`), '-subj', `/CN=${name}`);
  ssl('x509', '-req', '-in', join(ca, `${name}.csr`), '-CA', join(ca, 'ca.crt'), '-CAkey', join(ca, 'ca.key'), '-CAcreateserial', '-out', certOut, '-days', '1', '-extfile', join(ca, `${name}.ext`));
}
issue('smoke', 'subjectAltName=DNS:smoke,DNS:localhost\nextendedKeyUsage=serverAuth\n', join(run, 'tls/key.pem'), join(run, 'tls/cert.pem'));
issue('controller', 'extendedKeyUsage=clientAuth\n', join(ca, 'client.key'), join(ca, 'client.crt'));
copyFileSync(join(ca, 'ca.crt'), join(run, 'tls/ca.crt'));
writeFileSync(join(run, 'secrets/.generation'), '1\n');

const tls = { ca: readFileSync(join(ca, 'ca.crt')), cert: readFileSync(join(ca, 'client.crt')), key: readFileSync(join(ca, 'client.key')) };
const env = {
  PATH: process.env.PATH, HOME: process.env.HOME, TMPDIR: process.env.TMPDIR,
  A12S_COMPUTER: 'smoke', A12S_FLEET: 'lab', A12S_GENERATION: '1', A12S_HARNESS_KIND: kind,
  A12S_HARNESS_VERSION: kind === 'codex' ? '0.154.0' : '2.1.280', A12S_AUTONOMY: 'full', A12S_IMAGE_TIER: 'slim',
  A12S_BUNDLE: join(T, 'bundle'), A12S_CREDENTIALS: cred, A12S_HOME_ROOT: join(T, 'home'), A12S_RUN_DIR: run,
};
let daemon;
const startDaemon = () => {
  daemon = spawn(process.execPath, [join(root, 'dist/main.js')], { env, stdio: ['ignore', 'inherit', 'inherit'] });
};
const stopDaemon = () => new Promise((r) => (daemon.exitCode !== null ? r() : (daemon.once('exit', r), daemon.kill('SIGTERM'))));

function call(method, path, body, opts = tls) {
  return new Promise((resolve, reject) => {
    const req = request({ host: 'localhost', port: 7443, method, path, ...opts, headers: { 'content-type': 'application/json' } }, (res) => {
      let t = '';
      res.on('data', (d) => (t += d)).on('end', () => resolve({ status: res.statusCode, body: JSON.parse(t) }));
    });
    req.on('error', reject);
    req.end(body ? JSON.stringify(body) : undefined);
  });
}

/** Streams events (one turn, or everything after a cursor), printing trimmed lines, until `done(e)`. */
function follow(turnId, onEvent = () => {}, { after, done = (e) => e.type === 'turn.finished' } = {}) {
  const path = turnId ? `/v1/events?turn=${turnId}` : '/v1/events';
  const headers = after === undefined ? {} : { 'last-event-id': String(after) };
  let deltas = [];
  const flush = () => {
    if (deltas.length) console.log(`  #${deltas[0].seq}..#${deltas.at(-1).seq} text.delta x${deltas.length}: ${JSON.stringify(deltas.map((d) => d.data.text).join('')).slice(0, 120)}`);
    deltas = [];
  };
  return new Promise((resolve, reject) => {
    const req = request({ host: 'localhost', port: 7443, path, headers, ...tls }, (res) => {
      let buf = '';
      res.setEncoding('utf8').on('data', (chunk) => {
        buf += chunk;
        for (let i; (i = buf.indexOf('\n\n')) >= 0; ) {
          const block = buf.slice(0, i);
          const data = block.split('\n').find((l) => l.startsWith('data: '));
          buf = buf.slice(i + 2);
          if (!data || block.includes('event: ')) continue; // head / gap markers
          const e = JSON.parse(data.slice(6));
          if (e.type === 'text.delta') deltas.push(e);
          else (flush(), console.log(`  #${e.seq} ${e.type}${e.turn && !turnId ? ` [${e.turn}]` : ''} ${JSON.stringify(e.data).slice(0, 150)}`));
          onEvent(e);
          if (done(e)) (flush(), req.destroy(), resolve(e));
        }
      });
    });
    req.on('error', reject);
    req.end();
  });
}

async function ready() {
  for (let i = 0; i < 120; i++) {
    const s = await call('GET', '/v1/status').catch(() => null);
    if (s?.body.phase === 'ready') return s.body;
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error('computerd never became ready');
}

async function turn(label, session, prompt, onEvent) {
  const r = await call('POST', '/v1/turns', { requestId: randomUUID(), session, prompt });
  console.log(`\n== ${label}: POST /v1/turns -> ${r.status} ${JSON.stringify(r.body)}`);
  return { reply: r.body, finished: await follow(r.body.turnId, (e) => onEvent?.(e, r.body.turnId)) };
}

try {
  startDaemon();
  console.log('status:', JSON.stringify(await ready()));
  const noCert = await call('GET', '/v1/status', null, { ca: tls.ca }).then((r) => `HTTP ${r.status}`, (e) => e.code ?? e.message);
  console.log('without a client cert:', noCert);

  await turn('turn 1 (new session)', 'new', 'Reply with the single word pong.');
  const req2 = { requestId: randomUUID(), session: 'current', prompt: 'Reply with the single word pong again.' };
  const r2 = await call('POST', '/v1/turns', req2);
  console.log(`\n== turn 2 (current): POST -> ${r2.status} ${JSON.stringify(r2.body)}`);
  const retry = await call('POST', '/v1/turns', req2);
  console.log(`   same requestId again -> ${retry.status} ${JSON.stringify(retry.body)}`);
  await follow(r2.body.turnId);

  let stopped = false;
  await turn('turn 3 (long, then stop)', 'current', 'Write the numbers from one to five hundred as English words, one per line.', (e, turnId) => {
    if ((e.type === 'text.delta' || e.type === 'tool.started') && !stopped) {
      stopped = true;
      setTimeout(() => call('POST', `/v1/turns/${turnId}/stop`).then((s) => console.log(`  POST stop -> ${JSON.stringify(s.body)}`)), 3000);
    }
  });

  const r5 = await turn('turn 4 (restart_harness; turn 5 is the continuation)', 'current', 'Call the restart_harness tool of the a12s MCP server now, then reply with the single word restarting.');
  console.log('  ...following the stream after the turn:');
  let continuation = null;
  await follow(null, (e) => {
    if (e.type === 'turn.started') continuation = e.turn;
  }, { after: r5.finished.seq, done: (e) => e.type === 'turn.finished' && e.turn === continuation });

  console.log('\n== sleep/wake: SIGTERM computerd, start it again on the same home');
  await stopDaemon();
  startDaemon();
  console.log('status:', JSON.stringify(await ready()));
  await turn('turn 6 (after wake)', 'current', 'What single word did you reply with in your very first answer? Reply with just that word.');
  console.log('status:', JSON.stringify((await call('GET', '/v1/status')).body));
} finally {
  if (daemon) await stopDaemon();
  rmSync(T, { recursive: true, force: true });
  console.log(`\ncleaned up ${T}`);
}
