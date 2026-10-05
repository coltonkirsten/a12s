import assert from 'node:assert/strict';
import { createServer, get, request, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { Computer } from '../computer.js';
import { Journal } from '../journal.js';
import { apiHandler } from '../server.js';
import { testConfig, until } from './helpers.js';
import { StubAdapter } from './stub-adapter.js';

const cfg = testConfig();
const journal = new Journal(join(cfg.stateDir, 'journal.db'), cfg.computer);
const adapter = new StubAdapter();
const computer = new Computer(cfg, journal, adapter, {});
let server: Server;
let base: string;

after(() => server.close());

interface Frame {
  id?: string;
  event?: string;
  data: string;
}

/** Opens an SSE stream and collects frames until `n` arrived. */
function stream(path: string, headers: Record<string, string> = {}): { frames: Frame[]; close: () => void } {
  const frames: Frame[] = [];
  let buf = '';
  const req = get(`${base}${path}`, { headers }, (res) => {
    res.setEncoding('utf8').on('data', (chunk: string) => {
      buf += chunk;
      let i: number;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const block = buf.slice(0, i);
        buf = buf.slice(i + 2);
        const f: Frame = { data: '' };
        for (const line of block.split('\n')) {
          if (line.startsWith('id: ')) f.id = line.slice(4);
          else if (line.startsWith('event: ')) f.event = line.slice(7);
          else if (line.startsWith('data: ')) f.data = line.slice(6);
        }
        if (block.startsWith(':')) continue;
        frames.push(f);
      }
    });
  });
  req.on('error', () => {});
  return { frames, close: () => req.destroy() };
}

function post(path: string, body: unknown): Promise<{ status: number; body: any }> {
  return new Promise((resolve, reject) => {
    const req = request(`${base}${path}`, { method: 'POST', headers: { 'content-type': 'application/json' } }, (res) => {
      let text = '';
      res.setEncoding('utf8').on('data', (d: string) => (text += d));
      res.on('end', () => resolve({ status: res.statusCode ?? 0, body: JSON.parse(text) }));
    });
    req.on('error', reject);
    req.end(JSON.stringify(body));
  });
}

test('setup', async () => {
  await computer.start(); // journals lifecycle starting + ready (seq 1, 2)
  server = createServer(apiHandler(computer, journal));
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});

test('Last-Event-ID replays from the journal, then goes live without loss or duplicates', async () => {
  const s = stream('/v1/events', { 'last-event-id': '1' });
  await until(() => s.frames.length === 1);
  journal.append('diagnostic', { message: 'live' });
  await until(() => s.frames.length === 2);
  assert.deepEqual(s.frames.map((f) => f.id), ['2', '3']);
  const live = JSON.parse(s.frames[1]!.data);
  assert.deepEqual([live.v, live.computer, live.seq, live.type, live.data], [1, 'test', 3, 'diagnostic', { message: 'live' }]);
  s.close();
});

test('a cursor beyond the journal gets event: gap, then live events', async () => {
  const s = stream('/v1/events', { 'last-event-id': '9999' });
  await until(() => s.frames.length === 1);
  assert.equal(s.frames[0]!.event, 'gap');
  assert.deepEqual(JSON.parse(s.frames[0]!.data), { lastEventId: '9999', head: journal.lastSeq() });
  journal.append('diagnostic', { message: 'after gap' });
  await until(() => s.frames.length === 2);
  assert.equal(JSON.parse(s.frames[1]!.data).data.message, 'after gap');
  s.close();
});

test('POST /v1/turns, ?turn= replays only that turn, diagnostics and status', async () => {
  const bad = await post('/v1/turns', { requestId: 'r1', session: 'new' });
  assert.equal(bad.status, 400);
  const ok = await post('/v1/turns', { requestId: 'r1', session: 'new', prompt: 'hi' });
  assert.equal(ok.status, 200);
  const turnId = ok.body.turnId as string;
  assert.equal((await post('/v1/turns', { requestId: 'r2', session: 'new', prompt: 'hi' })).status, 409);
  assert.equal((await post('/v1/diagnostics', { message: 'egress denied', data: { host: 'evil.example' } })).status, 200);
  adapter.finish();
  const s = stream(`/v1/events?turn=${turnId}`);
  await until(() => s.frames.some((f) => JSON.parse(f.data).type === 'turn.finished'));
  const evs = s.frames.map((f) => JSON.parse(f.data));
  assert.deepEqual(evs.map((e) => e.type), ['turn.started', 'session.changed', 'text.delta', 'diagnostic', 'turn.finished']);
  assert.ok(evs.every((e) => e.turn === turnId));
  assert.deepEqual(evs[3].data, { message: 'egress denied', data: { host: 'evil.example' }, source: 'controller' });
  s.close();
  const st = await new Promise<any>((resolve) => get(`${base}/v1/status`, (res) => {
    let t = '';
    res.on('data', (d) => (t += d)).on('end', () => resolve(JSON.parse(t)));
  }));
  assert.deepEqual(Object.keys(st).sort(), ['activeTurn', 'credentialGeneration', 'harness', 'phase', 'resumePending', 'session']);
  assert.equal(st.phase, 'ready');
  assert.equal((await post(`/v1/turns/${turnId}/stop`, {})).body.status, 'completed');
});
