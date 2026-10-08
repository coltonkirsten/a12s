import assert from 'node:assert/strict';
import { readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { Computer, HttpError } from '../computer.js';
import type { Config } from '../config.js';
import { Journal } from '../journal.js';
import { testConfig, until } from './helpers.js';
import { StubAdapter } from './stub-adapter.js';

function setup(cfg: Config = testConfig()) {
  const journal = new Journal(join(cfg.stateDir, 'journal.db'), cfg.computer);
  const adapter = new StubAdapter();
  const computer = new Computer(cfg, journal, adapter, {});
  const types = (turn?: string) => journal.after(0, turn ?? null, 1000).map((e) => e.type);
  const finished = (turn: string) => journal.turnInfo(turn)?.finished;
  return { cfg, journal, adapter, computer, types, finished };
}

const status = (p: Promise<unknown>) => p.then(() => 200, (e: unknown) => (e instanceof HttpError ? e.status : 500));

test('turns: not ready is 503, busy is 409, requestId is idempotent and a changed body is 409', async () => {
  const { cfg, journal, adapter, computer, finished } = setup();
  const req = { requestId: 'r1', session: 'new', prompt: 'hi' };
  assert.equal(await status(computer.submitTurn(req)), 503);
  await computer.start();

  const [a, b] = await Promise.all([computer.submitTurn(req), computer.submitTurn(req)]);
  assert.deepEqual(a, b, 'a retry racing the first request gets the same reply');
  assert.equal(adapter.inputs.length, 1, 'the harness ran once');
  assert.equal(await status(computer.submitTurn({ ...req, prompt: 'other' })), 409);
  assert.equal(await status(computer.submitTurn({ ...req, requestId: 'r2' })), 409, 'busy');
  assert.equal(computer.status().phase, 'busy');

  adapter.finish();
  await until(() => finished(a.turnId) === 'completed');
  assert.deepEqual(await computer.submitTurn(req), a, 'same reply after the turn ended');

  // A new process over the same journal (container replaced) still knows the request.
  const again = new Computer(cfg, journal, new StubAdapter(), {});
  await again.start();
  assert.deepEqual(await again.submitTurn(req), a);
  assert.equal(await status(again.submitTurn({ ...req, cwd: '/elsewhere' })), 409);
});

test('session: new writes the record, current resumes it with resumed: true', async () => {
  const { cfg, journal, adapter, computer, finished } = setup();
  await computer.start();
  const first = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'remember kiwi' });
  adapter.finish();
  await until(() => finished(first.turnId) === 'completed');
  assert.deepEqual(JSON.parse(readFileSync(join(cfg.stateDir, 'session.json'), 'utf8')), { sessionId: first.sessionId, harness: 'claude-code', cwd: cfg.agentHome });

  const second = await computer.submitTurn({ requestId: 'r2', session: 'current', prompt: 'the word?' });
  assert.equal(second.sessionId, first.sessionId);
  const started = journal.after(0, second.turnId, 10).find((e) => e.type === 'turn.started');
  assert.equal(started?.data.resumed, true);
  assert.equal(adapter.inputs[1]?.sessionId, first.sessionId);
  adapter.finish();
  await until(() => finished(second.turnId) === 'completed');
  assert.equal(journal.after(0, null, 100).filter((e) => e.type === 'session.changed').length, 1);
});

test('resume failure: resume.failed at wake and at prompt, resumePending stays, no fresh session', async () => {
  const cfg = testConfig();
  writeFileSync(join(cfg.stateDir, 'session.json'), JSON.stringify({ sessionId: 's_gone', harness: 'claude-code', cwd: cfg.agentHome }));
  const { journal, adapter, computer, types } = setup(cfg);
  await computer.start();
  assert.ok(types().includes('resume.failed'));
  assert.equal(computer.status().resumePending, true);

  const t = await computer.submitTurn({ requestId: 'r1', session: 'current', prompt: 'continue' });
  assert.deepEqual(t.sessionId, 's_gone');
  assert.deepEqual(types(t.turnId), ['turn.started', 'resume.failed', 'turn.finished']);
  assert.equal(journal.after(0, t.turnId, 10).at(-1)?.data.status, 'failed');
  assert.equal(adapter.inputs[0]?.sessionId, 's_gone', 'never asked for a fresh session');
  assert.deepEqual([computer.status().resumePending, computer.status().session, computer.status().phase], [true, 's_gone', 'ready']);

  const fresh = await computer.submitTurn({ requestId: 'r2', session: 'new', prompt: 'start over' });
  assert.notEqual(fresh.sessionId, 's_gone');
  assert.equal(computer.status().resumePending, false);
});

test('stop is idempotent and the turn ends interrupted; unknown turns are 404', async () => {
  const { computer, finished } = setup();
  await computer.start();
  const t = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'long' });
  assert.deepEqual(computer.stopTurn(t.turnId), { turnId: t.turnId, status: 'stopping' });
  await until(() => finished(t.turnId) === 'interrupted');
  assert.deepEqual(computer.stopTurn(t.turnId), { turnId: t.turnId, status: 'interrupted' });
  assert.throws(() => computer.stopTurn('t_nope'), (e: HttpError) => e.status === 404);
});

test('restart_harness: restarts after the turn, owes a continuation, and runs it only for the Controller', async () => {
  const { cfg, journal, adapter, computer, types, finished } = setup();
  await computer.start();
  const t = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'build an MCP' });
  computer.requestRestart();
  assert.equal(adapter.restarts, 0, 'not while the turn runs');
  adapter.finish();
  await until(() => adapter.restarts === 1 && computer.status().continuation !== null);
  const owed = computer.status().continuation as string;
  assert.match(owed, /^c_[0-9A-Z]{26}$/);
  assert.equal(adapter.inputs.length, 1, 'computerd starts nothing itself');
  const restarted = journal.after(0, null, 100).find((e) => e.type === 'harness.restarted');
  assert.deepEqual([restarted?.data.continuation, restarted?.data.parentTurn], [owed, t.turnId]);
  assert.ok(types().indexOf('harness.restarted') > types().indexOf('turn.finished'));

  // Survives a computerd restart (owed state comes back from the journal).
  const again = new Computer(cfg, journal, adapter, {});
  await again.start();
  assert.equal(again.status().continuation, owed);

  // Only the owed id runs; the prompt is computerd's.
  assert.equal(await status(again.submitTurn({ requestId: 'c_other', session: 'current', prompt: '', continuation: true })), 409);
  const c = await again.submitTurn({ requestId: owed, session: 'current', prompt: '', continuation: true });
  assert.match(adapter.inputs[1]?.prompt ?? '', /^a12s restarted your harness at your request/);
  assert.equal(adapter.inputs[1]?.sessionId, t.sessionId);
  await until(() => again.status().continuation === null);
  const dispatched = journal.after(0, c.turnId, 100).find((e) => e.type === 'turn.dispatched');
  assert.equal(dispatched?.data.continuation, owed, 'the flag clears in the same append as turn.dispatched');
  adapter.finish();
  await until(() => finished(c.turnId) === 'completed');
  // A replay returns the same turn; it never runs twice.
  assert.deepEqual(await again.submitTurn({ requestId: owed, session: 'current', prompt: '', continuation: true }), c);
  assert.equal(adapter.inputs.length, 2);
  assert.equal(journal.after(0, null, 100).filter((e) => e.type === 'turn.started').at(-1)?.data.resumed, true);
});

test('a continuation that fails before dispatch is abandoned, not retried', async () => {
  const { journal, adapter, computer, finished } = setup();
  await computer.start();
  await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'x' });
  computer.requestRestart();
  adapter.finish();
  await until(() => computer.status().continuation !== null);
  const owed = computer.status().continuation as string;
  adapter.autoDispatch = false;
  const c = await computer.submitTurn({ requestId: owed, session: 'current', prompt: '', continuation: true });
  adapter.finish('failed');
  await until(() => finished(c.turnId) === 'failed');
  assert.equal(computer.status().continuation, null);
  assert.ok(journal.after(0, c.turnId, 100).some((e) => e.type === 'continuation.abandoned'));
  assert.equal(journal.turnSummary(c.turnId)?.dispatched, false);
});

test('turn.dispatched is journaled once, inbox is copied into turn.started, and the summary settles', async () => {
  const { journal, adapter, computer, finished } = setup();
  await computer.start();
  const inbox = { batch: 'b_1', pendingAfter: 0, messages: [{ id: 'm_1', seq: 1 }] };
  adapter.autoDispatch = false;
  const t = await computer.submitTurn({ requestId: 'b_1', session: 'new', prompt: 'p', inbox });
  assert.equal(journal.turnSummary(t.turnId)?.dispatched, false);
  adapter.dispatch();
  adapter.dispatch();
  await until(() => journal.turnSummary(t.turnId)?.dispatched === true);
  adapter.finish();
  await until(() => finished(t.turnId) === 'completed');
  const evs = journal.after(0, t.turnId, 100);
  assert.equal(evs.filter((e) => e.type === 'turn.dispatched').length, 1);
  assert.deepEqual(evs.find((e) => e.type === 'turn.started')?.data.inbox, inbox);
  assert.deepEqual(journal.turnSummary(t.turnId), { turnId: t.turnId, requestId: 'b_1', dispatched: true, resumeFailed: false, finished: 'completed', error: null, session: t.sessionId });
  // A changed inbox under the same requestId is a different body.
  assert.equal(await status(computer.submitTurn({ requestId: 'b_1', session: 'new', prompt: 'p', inbox: { ...inbox, batch: 'b_2' } })), 409);
});

test('lookupOnly returns a known turn, else refuses the requestId for good', async () => {
  const { journal, adapter, computer } = setup();
  await computer.start();
  const t = await computer.submitTurn({ requestId: 'b_1', session: 'new', prompt: 'p' });
  assert.deepEqual(await computer.submitTurn({ requestId: 'b_1', session: 'new', prompt: 'p', lookupOnly: true }), t);
  adapter.finish();
  assert.equal(await status(computer.submitTurn({ requestId: 'b_late', session: 'new', prompt: 'p', lookupOnly: true })), 404);
  assert.ok(journal.after(0, null, 100).some((e) => e.type === 'turn.refused' && e.data.requestId === 'b_late'));
  // The delayed original can never create that turn afterwards.
  assert.equal(await status(computer.submitTurn({ requestId: 'b_late', session: 'new', prompt: 'p' })), 404);
  assert.equal(adapter.inputs.length, 1);
});

test('shutdown closes admission and interrupts the active turn', async () => {
  const { computer, finished } = setup();
  await computer.start();
  const t = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'long' });
  await computer.shutdown(1000);
  assert.equal(finished(t.turnId), 'interrupted');
  assert.equal(await status(computer.submitTurn({ requestId: 'r2', session: 'new', prompt: 'x' })), 503);
});

test('acceptance is journaled before dispatch; a crash mid-turn ends it interrupted and never redispatches', async () => {
  const { cfg, journal, adapter, computer } = setup();
  await computer.start();
  let journaledBeforeDispatch = false;
  const original = adapter.startTurn.bind(adapter);
  adapter.startTurn = async (input) => {
    journaledBeforeDispatch = journal.findRequest('r1') !== undefined;
    return original(input);
  };
  const t = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'publish the release' });
  assert.ok(journaledBeforeDispatch);
  assert.equal(journal.turnInfo(t.turnId)?.session, t.sessionId, 'the new session id is recorded for idempotent replies');

  // computerd dies here: no turn.finished. A new process over the same journal:
  const fresh = new StubAdapter();
  const after = new Computer(cfg, journal, fresh, {});
  await after.start();
  assert.equal(journal.turnInfo(t.turnId)?.finished, 'interrupted');
  assert.equal(journal.after(0, t.turnId, 100).at(-1)?.data.error, 'computerd restarted');
  assert.deepEqual(await after.submitTurn({ requestId: 'r1', session: 'new', prompt: 'publish the release' }), t);
  assert.equal(fresh.inputs.length, 0, 'the retry did not reach the harness');
});

test('a damaged session record blocks session: current until session: new', async () => {
  const cfg = testConfig();
  writeFileSync(join(cfg.stateDir, 'session.json'), '{"sessionId": 42');
  const { adapter, computer, types } = setup(cfg);
  await computer.start();
  assert.deepEqual([computer.status().resumePending, computer.status().session], [true, null]);
  assert.ok(types().includes('resume.failed'));
  const t = await computer.submitTurn({ requestId: 'r1', session: 'current', prompt: 'continue the migration' });
  assert.deepEqual(types(t.turnId), ['turn.started', 'resume.failed', 'turn.finished']);
  assert.equal(adapter.inputs.length, 0, 'never started a fresh session');
  await computer.submitTurn({ requestId: 'r2', session: 'new', prompt: 'start over' });
  adapter.finish();
  assert.equal(computer.status().resumePending, false);
});

test('session record loading: missing is fresh, wrong shape is damaged', async () => {
  const { loadSessionRecord } = await import('../computer.js');
  const cfg = testConfig();
  const p = join(cfg.stateDir, 'session.json');
  assert.deepEqual(loadSessionRecord(p), { record: null, damaged: null });
  writeFileSync(p, '{"sessionId":"s","harness":"vim","cwd":"/home/agent"}');
  assert.match(loadSessionRecord(p).damaged ?? '', /damaged/);
  writeFileSync(p, '{"sessionId":"s","harness":"codex","cwd":"/home/agent"}');
  assert.deepEqual(loadSessionRecord(p), { record: { sessionId: 's', harness: 'codex', cwd: '/home/agent' }, damaged: null });
});

test('stop while the harness is still starting reaches the adapter and ends the turn interrupted', async () => {
  const { adapter, computer, finished } = setup();
  await computer.start();
  adapter.stallStart = true;
  const reply = computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'x' });
  await until(() => computer.status().activeTurn !== null);
  computer.stopTurn(String(computer.status().activeTurn));
  const t = await reply;
  assert.equal(finished(t.turnId), 'interrupted');
  assert.equal(computer.status().phase, 'ready');
});

test('credential generation is acknowledged only once applied; a busy turn defers it to the idle boundary', async () => {
  const cfg = testConfig();
  writeFileSync(join(cfg.secretsDir, '.generation'), '1\n');
  const journal = new Journal(join(cfg.stateDir, 'journal.db'), cfg.computer);
  const adapter = new StubAdapter();
  const computer = new Computer(cfg, journal, adapter, {}, 20);
  await computer.start();
  assert.equal(computer.status().credentialGeneration, 1);

  const t = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'long' });
  writeFileSync(join(cfg.secretsDir, '.generation'), '2\n');
  await new Promise((r) => setTimeout(r, 100));
  assert.equal(computer.status().credentialGeneration, 1, 'not while the harness still runs on the old credentials');
  assert.equal(adapter.restarts, 0);
  adapter.finish();
  await until(() => computer.status().credentialGeneration === 2);
  assert.equal(adapter.restarts, 1);
  assert.equal(journal.turnInfo(t.turnId)?.finished, 'completed');

  writeFileSync(join(cfg.secretsDir, '.generation'), '3\n');
  await until(() => computer.status().credentialGeneration === 3);
  assert.equal(adapter.restarts, 2, 'idle: restarted then acknowledged');
  await computer.shutdown(100);
});

test('browser tier: harness env carries NODE_PATH for Playwright', async () => {
  const { harnessEnv } = await import('../adapters/spawn.js');
  const deps = (imageTier: string) => ({ cfg: testConfig({ imageTier }), credentialEnv: () => ({}), mcpServers: {} });
  assert.equal(harnessEnv(deps('browser')).NODE_PATH, '/opt/a12s/playwright-mcp/node_modules');
  assert.equal(harnessEnv(deps('slim')).NODE_PATH, undefined);
});

test('a stop that lands during the pre-dispatch credential restart cancels without dispatching', async () => {
  const cfg = testConfig();
  writeFileSync(join(cfg.secretsDir, '.generation'), '1\n');
  const { adapter, computer, finished } = setup(cfg);
  await computer.start();
  writeFileSync(join(cfg.secretsDir, '.generation'), '2\n');
  let release!: () => void;
  adapter.restartGate = new Promise((r) => (release = r));
  const reply = computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'publish' });
  await until(() => adapter.restarts === 1);
  computer.stopTurn(String(computer.status().activeTurn));
  release();
  const t = await reply;
  assert.equal(finished(t.turnId), 'interrupted');
  assert.equal(adapter.inputs.length, 0, 'the prompt never reached the harness');
});

test('failed credential wiring never advances the acknowledged generation', async () => {
  const cfg = testConfig({ credentials: ['github'] });
  writeFileSync(join(cfg.secretsDir, 'github'), 'ghp_1');
  writeFileSync(join(cfg.secretsDir, '.generation'), '1\n');
  const journal = new Journal(join(cfg.stateDir, 'journal.db'), cfg.computer);
  const adapter = new StubAdapter();
  const computer = new Computer(cfg, journal, adapter, {}, 20);
  await computer.start();
  assert.equal(computer.status().credentialGeneration, 1);

  // Generation 2 arrives without the declared credential: idle apply, prompt-time and startup all keep 1.
  rmSync(join(cfg.secretsDir, 'github'));
  writeFileSync(join(cfg.secretsDir, '.generation'), '2\n');
  await new Promise((r) => setTimeout(r, 150));
  assert.equal(computer.status().credentialGeneration, 1);
  assert.equal(adapter.restarts, 0);
  const t = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'x' });
  adapter.finish();
  await until(() => journal.turnInfo(t.turnId)?.finished === 'completed');
  assert.equal(computer.status().credentialGeneration, 1);
  assert.ok(journal.after(0, null, 1000).some((e) => e.type === 'diagnostic' && e.data.message === 'credential wiring failed'));
  await computer.shutdown(100);

  const again = new Computer(cfg, journal, new StubAdapter(), {}, 20);
  await again.start();
  assert.equal(again.status().credentialGeneration, null, 'startup keeps the previous (none)');

  writeFileSync(join(cfg.secretsDir, 'github'), 'ghp_2');
  writeFileSync(join(cfg.secretsDir, '.generation'), '3\n');
  await until(() => again.status().credentialGeneration === 3);
  await again.shutdown(100);
});
