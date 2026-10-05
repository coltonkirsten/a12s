import assert from 'node:assert/strict';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { ResumeError, type AdapterEvent, type TurnInput } from '../adapters/adapter.js';
import { ClaudeAdapter } from '../adapters/claude.js';
import { collect, fixtures, testConfig } from './helpers.js';

const fake = join(fixtures, 'fake-claude.js');

function setup(mode: string, autonomy: 'full' | 'restricted' = 'full', timing = { termAfterMs: 10_000, killAfterMs: 5_000 }) {
  const cfg = testConfig({ autonomy });
  const argsFile = join(cfg.runDir, 'args.jsonl');
  const adapter = new ClaudeAdapter({ cfg, credentialEnv: () => ({ FAKE_MODE: mode, FAKE_ARGS_FILE: argsFile }), mcpServers: {} }, fake, timing);
  const input: TurnInput = {
    prompt: 'reply with pong',
    sessionId: null,
    cwd: cfg.agentHome,
    model: null,
    autonomy,
    mcpServers: { a12s: { command: '/bin/true', args: ['x'] } },
  };
  const args = () => JSON.parse(readFileSync(argsFile, 'utf8').trim().split('\n').at(-1) ?? '[]') as string[];
  return { cfg, adapter, input, args };
}

const types = (evs: AdapterEvent[]) => evs.map((e) => e.type);

test('claude: maps deltas, tools and the result; passes the pinned flags', async () => {
  const { adapter, input, args } = setup('normal');
  const run = await adapter.startTurn(input);
  const evs = await collect(run.events);
  assert.deepEqual(types(evs), ['text.delta', 'text.delta', 'tool.started', 'tool.finished', 'turn.finished']);
  assert.equal(evs.filter((e) => e.type === 'text.delta').map((e) => (e.data as { text: string }).text).join(''), 'pong');
  assert.deepEqual(evs[2]?.data, { id: 'toolu_1', name: 'Bash', input: { command: 'echo hi' } });
  assert.deepEqual(evs[3]?.data, { id: 'toolu_1', name: 'Bash', isError: false, output: 'hi' });
  const fin = evs.at(-1) as Extract<AdapterEvent, { type: 'turn.finished' }>;
  assert.equal(fin.data.status, 'completed');
  assert.equal(fin.data.usage?.outputTokens, 2);
  const a = args();
  assert.match(run.sessionId, /^[0-9a-f-]{36}$/);
  for (const f of [`--session-id=${run.sessionId}`, '--setting-sources=user,project,local', '--strict-mcp-config', '--include-partial-messages', '--allow-dangerously-skip-permissions']) {
    assert.ok(a.includes(f), f);
  }
  assert.equal(a[a.indexOf('--permission-mode') + 1], 'bypassPermissions');
  assert.ok(a[a.indexOf('--mcp-config') + 1]?.includes('"a12s"'));
});

test('claude: restricted maps to dontAsk + no prompts, and denials become permission events once each', async () => {
  const { adapter, input, args } = setup('deny', 'restricted');
  const evs = await collect((await adapter.startTurn(input)).events);
  const perms = evs.filter((e) => e.type === 'permission').map((e) => (e.data as { detail: { toolUseId: string } }).detail.toolUseId);
  assert.deepEqual(perms, ['toolu_3', 'toolu_2']);
  const a = args();
  assert.equal(a[a.indexOf('--permission-mode') + 1], 'dontAsk');
  assert.equal(a[a.indexOf('--permission-prompts') + 1], 'none');
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
  const { adapter, input } = setup('ignore-term', 'full', { termAfterMs: 200, killAfterMs: 200 });
  const run = await adapter.startTurn(input);
  setTimeout(() => void adapter.stop(), 300);
  const evs = await collect(run.events);
  assert.equal((evs.at(-1) as Extract<AdapterEvent, { type: 'turn.finished' }>).data.status, 'interrupted');
});
