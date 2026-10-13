import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { ResumeError, type AdapterEvent, type TaskEvent, type TurnInput } from '../adapters/adapter.js';
import { CodexAdapter, mcpConfigArgs } from '../adapters/codex.js';
import { collect, fixtures, testConfig, until } from './helpers.js';

const fake = join(fixtures, 'fake-codex.js');

function setup(mode: string, env: Record<string, string> = {}) {
  const cfg = testConfig({ harness: { kind: 'codex', version: '0.0.0', model: null } });
  const logFile = join(cfg.runDir, 'codex.jsonl');
  const mcp = { a12s: { command: '/usr/bin/node', args: ['/opt/x.js'], env: { A12S_SOCK: '/run/s.sock' } } };
  const adapter = new CodexAdapter({ cfg, credentialEnv: () => ({ FAKE_MODE: mode, FAKE_LOG: logFile, ...env }), mcpServers: mcp }, fake, { termAfterMs: 300, killAfterMs: 300 });
  const input: TurnInput = { prompt: 'reply with pong', sessionId: null, cwd: cfg.agentHome, model: 'gpt-x', mcpServers: mcp };
  after(() => adapter.close());
  const tasks: TaskEvent[] = [];
  adapter.listen({ backgroundTurn: () => assert.fail('codex never starts a turn by itself'), taskEvent: (ev) => tasks.push(ev) });
  const log = () => readFileSync(logFile, 'utf8').trim().split('\n').map((l) => JSON.parse(l) as Record<string, any>);
  return { adapter, input, log, tasks };
}

const finished = (evs: AdapterEvent[]) => (evs.at(-1) as Extract<AdapterEvent, { type: 'turn.finished' }>).data;

test('codex: mcp servers become -c overrides', () => {
  assert.deepEqual(mcpConfigArgs({ a12s: { command: '/n', args: ['a b'], env: { K: 'v' }, cwd: '/home/agent' } }), [
    '-c', 'mcp_servers.a12s.command="/n"', '-c', 'mcp_servers.a12s.args=["a b"]', '-c', 'mcp_servers.a12s.cwd="/home/agent"', '-c', 'mcp_servers.a12s.env.K="v"',
  ]);
});

test('codex: handshake, thread/start, events, and a granted approval', async () => {
  const { adapter, input, log } = setup('normal');
  const run = await adapter.startTurn(input);
  assert.equal(run.sessionId, 'thr_1');
  const evs = await collect(run.events);
  assert.deepEqual(evs.map((e) => e.type), ['turn.dispatched', 'tool.started', 'permission', 'tool.finished', 'text.delta', 'text.delta', 'turn.finished']);
  assert.deepEqual(evs[2]?.data, { decision: 'allow', kind: 'commandExecution', detail: { command: 'ls', cwd: '/' } });
  assert.equal(finished(evs).status, 'completed');
  assert.deepEqual(finished(evs).usage, { thread: { totalTokens: 9 }, last: { totalTokens: 5 } });
  const msgs = log();
  assert.ok((msgs[0]?.argv as string[]).includes('app-server'));
  assert.deepEqual(msgs.slice(1, 3).map((m) => m.method), ['initialize', 'initialized']);
  assert.equal(msgs[1]?.params.clientInfo.name, 'a12s');
  const start = msgs.find((m) => m.method === 'thread/start');
  assert.deepEqual([start?.params.approvalPolicy, start?.params.sandbox, start?.params.model], ['never', 'danger-full-access', 'gpt-x']);
  assert.deepEqual(msgs.find((m) => m.id === 'srv_1')?.result, { decision: 'accept' });
  await adapter.close();
});

test('codex: never + danger-full-access; resume of a loaded thread skips thread/resume', async () => {
  const { adapter, input, log } = setup('normal');
  const first = await adapter.startTurn(input);
  await collect(first.events);
  const second = await adapter.startTurn({ ...input, sessionId: first.sessionId });
  assert.equal(finished(await collect(second.events)).status, 'completed');
  const msgs = log();
  const start = msgs.find((m) => m.method === 'thread/start');
  assert.deepEqual([start?.params.approvalPolicy, start?.params.sandbox], ['never', 'danger-full-access']);
  assert.equal(msgs.filter((m) => m.method === 'thread/resume').length, 0);
  assert.equal(msgs.filter((m) => m.method === 'turn/start').length, 2);
  await adapter.close();
});

test('codex: precheck and resume failures surface as reasons / ResumeError', async () => {
  const { adapter, input } = setup('normal');
  assert.equal(await adapter.precheck('thr_9'), null);
  assert.match((await adapter.precheck('thr_missing')) ?? '', /no rollout/);
  await assert.rejects(adapter.startTurn({ ...input, sessionId: 'thr_missing' }), ResumeError);
  await adapter.close();
});

test('codex: stop sends turn/interrupt and the turn ends interrupted', async () => {
  const { adapter, input, log } = setup('hang');
  const run = await adapter.startTurn(input);
  setTimeout(() => void adapter.stop(), 100);
  assert.equal(finished(await collect(run.events)).status, 'interrupted');
  assert.ok(log().some((m) => m.method === 'turn/interrupt'));
  await adapter.close();
});

test('codex: an app-server that ignores interrupt is killed; turn ends interrupted, restart resumes cleanly', async () => {
  const { adapter, input } = setup('ignore-interrupt');
  const run = await adapter.startTurn(input);
  setTimeout(() => void adapter.stop(), 100);
  assert.equal(finished(await collect(run.events)).status, 'interrupted');
  await adapter.restart();
  assert.equal(await adapter.precheck(run.sessionId), null);
  await adapter.close();
});

test('codex: the configured model goes on every turn/start, also after a precheck resume', async () => {
  const { adapter, input, log } = setup('normal');
  assert.equal(await adapter.precheck('thr_7'), null);
  await collect((await adapter.startTurn({ ...input, sessionId: 'thr_7' })).events);
  assert.equal(log().find((m) => m.method === 'turn/start')?.params.model, 'gpt-x');
});

test('codex: a stalled request hits its deadline; stop during startup kills the app-server', async () => {
  const cfg = testConfig({ harness: { kind: 'codex', version: '0', model: null } });
  const deps = { cfg, credentialEnv: () => ({ FAKE_MODE: 'stall' }), mcpServers: {} };
  const input: TurnInput = { prompt: 'p', sessionId: null, cwd: cfg.agentHome, model: null, mcpServers: {} };
  const slow = new CodexAdapter(deps, fake, { termAfterMs: 300, killAfterMs: 300 }, 300);
  after(() => slow.close());
  await assert.rejects(slow.startTurn(input), /did not answer turn\/start within 300ms/);

  const stoppable = new CodexAdapter(deps, fake, { termAfterMs: 300, killAfterMs: 300 }, 60_000);
  after(() => stoppable.close());
  const started = Date.now();
  const p = stoppable.startTurn(input);
  setTimeout(() => void stoppable.stop(), 200);
  await assert.rejects(p, /stopped before the turn started/);
  assert.ok(Date.now() - started < 5000);
});

test('codex: a command still running at the turn end is a background task until its late completion', async () => {
  const { adapter, input, tasks } = setup('normal');
  const run = await adapter.startTurn({ ...input, prompt: 'bg' });
  const evs = await collect(run.events);
  assert.deepEqual(evs.map((e) => e.type).slice(-2), ['task.started', 'turn.finished']);
  assert.equal(adapter.backgroundTasks(), 1);
  await until(() => tasks.length === 1);
  assert.deepEqual(tasks[0]?.data, { id: (evs.at(-2) as TaskEvent).data.id, kind: 'shell', description: 'sleep 1; make', status: 'completed' });
  assert.equal(adapter.backgroundTasks(), 0);
});

test('codex: stop and a new thread end the background terminals with the app-server', async () => {
  const { adapter, input, tasks, log } = setup('normal', { FAKE_BG_MS: '60000' });
  await collect((await adapter.startTurn({ ...input, prompt: 'bg' })).events);
  await adapter.stop();
  await until(() => tasks.length === 1);
  assert.equal((tasks[0]?.data as { status: string }).status, 'killed');
  const run = await adapter.startTurn({ ...input, prompt: 'bg' });
  await collect(run.events);
  await collect((await adapter.startTurn(input)).events);
  await until(() => tasks.length === 2);
  assert.equal(adapter.backgroundTasks(), 0);
  assert.equal(log().filter((m) => Array.isArray(m.argv)).length, 3, 'one app-server per session with background work');
});

test('codex: a failed turn leaves no background tasks behind', async () => {
  const { adapter, input, tasks } = setup('normal', { FAKE_BG_MS: '60000', FAKE_BG_STATUS: 'failed' });
  const evs = await collect((await adapter.startTurn({ ...input, prompt: 'bg' })).events);
  assert.equal(finished(evs).status, 'failed');
  assert.ok(!evs.some((e) => e.type === 'task.started'));
  assert.deepEqual([adapter.backgroundTasks(), tasks.length], [0, 0]);
});
