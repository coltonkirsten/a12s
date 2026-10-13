import assert from 'node:assert/strict';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { after, test } from 'node:test';
import { ResumeError, type AdapterEvent, type RunningTurn, type TaskEvent, type TurnInput } from '../adapters/adapter.js';
import { ClaudeAdapter } from '../adapters/claude.js';
import { collect, fixtures, testConfig, until } from './helpers.js';

const fake = join(fixtures, 'fake-claude.js');
const adapters: ClaudeAdapter[] = [];
after(() => Promise.all(adapters.map((a) => a.close())));

function setup(mode: string, timing = { termAfterMs: 10_000, killAfterMs: 5_000 }, env: Record<string, string> = {}) {
  const cfg = testConfig();
  const argsFile = join(cfg.runDir, 'args.jsonl');
  const adapter = new ClaudeAdapter({ cfg, credentialEnv: () => ({ FAKE_MODE: mode, FAKE_ARGS_FILE: argsFile, ...env }), mcpServers: {} }, fake, timing);
  adapters.push(adapter);
  const background: RunningTurn[] = [];
  const tasks: TaskEvent[] = [];
  adapter.listen({ backgroundTurn: (run) => background.push(run), taskEvent: (ev) => tasks.push(ev) });
  const input: TurnInput = {
    prompt: 'reply with pong',
    sessionId: null,
    cwd: cfg.agentHome,
    model: null,
    mcpServers: { a12s: { command: '/bin/true', args: ['x'], alwaysLoad: true } },
  };
  const spawns = () => readFileSync(argsFile, 'utf8').trim().split('\n').length;
  const args = () => JSON.parse(readFileSync(argsFile, 'utf8').trim().split('\n').at(-1) ?? '[]') as string[];
  const transcript = (id: string) => {
    const dir = join(cfg.claudeConfigDir, 'projects', '-home-agent');
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, `${id}.jsonl`), '{"type":"user"}\n');
  };
  return { cfg, adapter, input, args, spawns, transcript, background, tasks };
}

const types = (evs: AdapterEvent[]) => evs.map((e) => e.type);
const text = (evs: AdapterEvent[]) => evs.map((e) => (e.type === 'text.delta' ? e.data.text : '')).join('');
const finished = (evs: AdapterEvent[]) => (evs.at(-1) as Extract<AdapterEvent, { type: 'turn.finished' }>).data;

test('claude: maps deltas, tools and the result; passes the pinned flags', async () => {
  const { adapter, input, args } = setup('normal');
  const run = await adapter.startTurn(input);
  const evs = await collect(run.events);
  assert.deepEqual(types(evs), ['turn.dispatched', 'text.delta', 'text.delta', 'tool.started', 'tool.finished', 'turn.finished']);
  assert.equal(evs.filter((e) => e.type === 'text.delta').map((e) => (e.data as { text: string }).text).join(''), 'pong');
  assert.deepEqual(evs[3]?.data, { id: 'toolu_1', name: 'Bash', input: { command: 'echo hi' } });
  assert.deepEqual(evs[4]?.data, { id: 'toolu_1', name: 'Bash', isError: false, output: 'hi' });
  const fin = evs.at(-1) as Extract<AdapterEvent, { type: 'turn.finished' }>;
  assert.equal(fin.data.status, 'completed');
  assert.equal(fin.data.usage?.outputTokens, 2);
  const a = args();
  assert.match(run.sessionId, /^[0-9a-f-]{36}$/);
  for (const f of [`--session-id=${run.sessionId}`, '--setting-sources=user,project,local', '--include-partial-messages', '--allow-dangerously-skip-permissions', '--replay-user-messages']) {
    assert.ok(a.includes(f), f);
  }
  assert.ok(!a.includes('--strict-mcp-config'));
  assert.equal(a[a.indexOf('--permission-mode') + 1], 'bypassPermissions');
  assert.ok(a[a.indexOf('--mcp-config') + 1]?.includes('"a12s"'));
  assert.ok(a[a.indexOf('--mcp-config') + 1]?.includes('"alwaysLoad":true'), 'the system tools are never deferred');
});

test('claude: denials become permission events once each', async () => {
  const { adapter, input } = setup('deny');
  const evs = await collect((await adapter.startTurn(input)).events);
  const perms = evs.filter((e) => e.type === 'permission').map((e) => (e.data as { detail: { toolUseId: string } }).detail.toolUseId);
  assert.deepEqual(perms, ['toolu_3', 'toolu_2']);
});

test('claude: resume requires the transcript; with it the SDK gets --resume', async () => {
  const { cfg, adapter, input, args } = setup('normal');
  const id = '0f8fad5b-d9cb-469f-a165-70867728950e';
  await assert.rejects(adapter.startTurn({ ...input, sessionId: id }), ResumeError);
  const dir = join(cfg.claudeConfigDir, 'projects', '-home-agent');
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, `${id}.jsonl`), '{"type":"user"}\n');
  assert.equal(await adapter.precheck(id), null);
  const run = await adapter.startTurn({ ...input, sessionId: id });
  assert.equal(run.sessionId, id);
  await collect(run.events);
  assert.ok(args().includes(`--resume=${id}`));
});

test('claude: stop interrupts and the turn ends interrupted', async () => {
  const { adapter, input } = setup('hang');
  const run = await adapter.startTurn(input);
  setTimeout(() => void adapter.stop(), 300);
  const evs = await collect(run.events);
  assert.deepEqual((evs.at(-1) as Extract<AdapterEvent, { type: 'turn.finished' }>).data.status, 'interrupted');
});

test('claude: a harness that ignores interrupt and SIGTERM is killed, turn ends interrupted', async () => {
  const { adapter, input } = setup('ignore-term', { termAfterMs: 200, killAfterMs: 200 });
  const run = await adapter.startTurn(input);
  setTimeout(() => void adapter.stop(), 300);
  const evs = await collect(run.events);
  assert.equal((evs.at(-1) as Extract<AdapterEvent, { type: 'turn.finished' }>).data.status, 'interrupted');
});

test('claude: a resume that comes up in another session is stopped and fails with resume.failed', async () => {
  const { cfg, adapter, input } = setup('wrong-session');
  const id = '0f8fad5b-d9cb-469f-a165-70867728950e';
  const dir = join(cfg.claudeConfigDir, 'projects', '-home-agent');
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, `${id}.jsonl`), '{"type":"user"}\n');
  const evs = await collect((await adapter.startTurn({ ...input, sessionId: id })).events);
  assert.deepEqual(evs.map((e) => e.type), ['resume.failed', 'turn.finished'], 'a wrong-session init is a resume failure, never a receipt');
  const fin = (evs.at(-1) as Extract<AdapterEvent, { type: 'turn.finished' }>).data;
  assert.equal(fin.status, 'failed');
  assert.match(fin.error ?? '', /started session aaaaaaaa/);
});

test('claude: receipt needs the user-message echo and a passing init, in either order', async () => {
  for (const [mode, want] of [['replay-first', true], ['no-replay', false]] as const) {
    const { adapter, input } = setup(mode);
    const evs = await collect((await adapter.startTurn(input)).events);
    assert.equal(evs.filter((e) => e.type === 'turn.dispatched').length, want ? 1 : 0, mode);
    if (want) assert.equal(evs[0]?.type, 'turn.dispatched', 'before any model output');
  }
});

test('claude: one process serves consecutive turns of a session', async () => {
  const { adapter, input, spawns } = setup('normal');
  const first = await adapter.startTurn(input);
  await collect(first.events);
  const second = await adapter.startTurn({ ...input, sessionId: first.sessionId });
  assert.equal(finished(await collect(second.events)).status, 'completed');
  assert.equal(spawns(), 1, 'no new process, so background work survives');
});

test('claude: background work outlives its turn; the CLI starts a turn by itself when it ends', async () => {
  const { adapter, input, background, tasks } = setup('normal');
  const run = await adapter.startTurn({ ...input, prompt: 'bg 300' });
  const evs = await collect(run.events);
  assert.deepEqual(types(evs), ['turn.dispatched', 'tool.started', 'task.started', 'tool.finished', 'text.delta', 'turn.finished']);
  assert.deepEqual(evs[2]?.data, { id: 'task_1', kind: 'local_agent', description: 'sleep 300' });
  assert.equal(adapter.backgroundTasks(), 1, 'still running after the turn ended');
  await until(() => background.length === 1);
  const own = await collect(background[0]!.events);
  assert.equal(background[0]!.sessionId, run.sessionId);
  assert.deepEqual(types(own), ['text.delta', 'turn.finished'], 'no receipt, and no sub-agent output from between turns');
  assert.equal(text(own), 'task task_1 finished');
  assert.deepEqual(tasks.map((t) => [t.type, t.data.id, 'status' in t.data ? t.data.status : null]), [['task.finished', 'task_1', 'completed']]);
  assert.equal(adapter.backgroundTasks(), 0);
});

test('claude: a prompt while background work runs starts at once', async () => {
  const { adapter, input } = setup('normal');
  const run = await adapter.startTurn({ ...input, prompt: 'bg 5000' });
  await collect(run.events);
  const t0 = Date.now();
  const next = await adapter.startTurn({ ...input, sessionId: run.sessionId, prompt: 'what is 17*3' });
  const evs = await collect(next.events);
  assert.equal(finished(evs).status, 'completed');
  assert.ok(Date.now() - t0 < 2000, 'did not wait for the background task');
  assert.equal(adapter.backgroundTasks(), 1);
});

test('claude: a prompt pushed while the CLI runs its own turn waits for it, and nothing is mis-attributed', async () => {
  const { adapter, input, background } = setup('normal', undefined, { FAKE_SELF_MS: '300' });
  const run = await adapter.startTurn({ ...input, prompt: 'bg 50' });
  await collect(run.events);
  await until(() => background.length === 1);
  // computerd races the CLI: the prompt goes in while the self-started turn is still open.
  const next = await adapter.startTurn({ ...input, sessionId: run.sessionId });
  const [own, mine] = await Promise.all([collect(background[0]!.events), collect(next.events)]);
  assert.equal(text(own), 'task task_1 finished');
  assert.deepEqual(types(own), ['text.delta', 'turn.finished']);
  assert.equal(text(mine), 'pong');
  assert.equal(mine[0]?.type, 'turn.dispatched');
  assert.equal(finished(mine).status, 'completed');
});

test('claude: stop ends the turn and the session with its background work', async () => {
  const { adapter, input, tasks, spawns } = setup('normal');
  const run = await adapter.startTurn({ ...input, prompt: 'bg 10000' });
  await collect(run.events);
  assert.equal(adapter.backgroundTasks(), 1);
  await adapter.stop();
  await until(() => tasks.some((t) => t.type === 'task.finished'));
  assert.equal(adapter.backgroundTasks(), 0);
  assert.deepEqual(tasks.map((t) => [t.type, 'status' in t.data ? t.data.status : null]), [['task.finished', 'stopped']]);
  const again = await adapter.startTurn({ ...input, sessionId: run.sessionId }).catch((e: Error) => e);
  assert.ok(again instanceof ResumeError, 'the fake writes no transcript: the next turn has to resume in a new process');
  assert.equal(spawns(), 1);
});

test('claude: a new session or a restart ends the live one and its background work', async () => {
  const { adapter, input, tasks, spawns } = setup('normal');
  const run = await adapter.startTurn({ ...input, prompt: 'bg 10000' });
  await collect(run.events);
  const fresh = await adapter.startTurn(input);
  assert.notEqual(fresh.sessionId, run.sessionId);
  await collect(fresh.events);
  assert.equal(spawns(), 2);
  assert.equal(tasks.filter((t) => t.type === 'task.finished').length, 1);
  const bg = await adapter.startTurn({ ...input, sessionId: fresh.sessionId, prompt: 'bg 10000' });
  await collect(bg.events);
  await adapter.restart();
  assert.equal(adapter.backgroundTasks(), 0);
  assert.equal(tasks.filter((t) => t.type === 'task.finished').length, 2);
});

test('claude: a crash mid-turn fails the turn and kills its tasks; the next turn resumes the session in a new process', async () => {
  const { adapter, input, args, spawns, transcript } = setup('normal');
  const bg = await adapter.startTurn({ ...input, prompt: 'bg 10000' });
  await collect(bg.events);
  const run = await adapter.startTurn({ ...input, sessionId: bg.sessionId, prompt: 'crash' });
  const evs = await collect(run.events);
  assert.deepEqual(types(evs), ['turn.dispatched', 'task.finished', 'diagnostic', 'turn.finished']);
  assert.equal((evs[1] as TaskEvent).data.id, 'task_1');
  assert.equal(finished(evs).status, 'failed');
  transcript(bg.sessionId);
  const next = await adapter.startTurn({ ...input, sessionId: bg.sessionId });
  assert.equal(finished(await collect(next.events)).status, 'completed');
  assert.equal(spawns(), 2);
  assert.ok(args().includes(`--resume=${bg.sessionId}`));
});

test('claude: a CLI without lifecycle frames still delimits turns by init and result', async () => {
  const { adapter, input } = setup('no-lifecycle');
  const first = await adapter.startTurn(input);
  assert.deepEqual(types(await collect(first.events)), ['turn.dispatched', 'text.delta', 'text.delta', 'tool.started', 'tool.finished', 'turn.finished']);
  const second = await adapter.startTurn({ ...input, sessionId: first.sessionId });
  assert.equal(finished(await collect(second.events)).status, 'completed');
});
