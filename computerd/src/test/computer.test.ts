import assert from 'node:assert/strict';
import { readFileSync, writeFileSync } from 'node:fs';
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

test('restart_harness: restarts after the turn, resumes, emits harness.restarted, continues', async () => {
  const { journal, adapter, computer, types } = setup();
  await computer.start();
  const t = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'build an MCP' });
  computer.requestRestart();
  assert.equal(adapter.restarts, 0, 'not while the turn runs');
  adapter.finish();
  await until(() => adapter.inputs.length === 2);
  assert.equal(adapter.restarts, 1);
  assert.match(adapter.inputs[1]?.prompt ?? '', /^a12s restarted your harness at your request/);
  assert.equal(adapter.inputs[1]?.sessionId, t.sessionId);
  const all = types();
  assert.ok(all.indexOf('harness.restarted') > all.indexOf('turn.finished'));
  assert.equal(journal.after(0, null, 100).filter((e) => e.type === 'turn.started').at(-1)?.data.resumed, true);
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
