import assert from 'node:assert/strict';
import { join } from 'node:path';
import { test } from 'node:test';
import { ClaudeAdapter } from '../adapters/claude.js';
import { Computer } from '../computer.js';
import { Journal } from '../journal.js';
import { fixtures, testConfig, until } from './helpers.js';

test('live session: background work survives the turn, a Message meanwhile runs at once, its end is a background turn', async () => {
  const cfg = testConfig();
  const journal = new Journal(join(cfg.stateDir, 'journal.db'), cfg.computer);
  const adapter = new ClaudeAdapter({ cfg, credentialEnv: () => ({ FAKE_MODE: 'normal' }), mcpServers: {} }, join(fixtures, 'fake-claude.js'));
  const computer = new Computer(cfg, journal, adapter, {});
  await computer.start();
  const finished = (turn: string) => journal.turnInfo(turn)?.finished;
  const turnText = (turn: string) => journal.after(0, turn, 100).filter((e) => e.type === 'text.delta').map((e) => e.data.text).join('');

  const launch = await computer.submitTurn({ requestId: 'b_1', session: 'new', prompt: 'bg 400' });
  await until(() => finished(launch.turnId) === 'completed');
  assert.deepEqual([computer.status().phase, computer.status().background], ['ready', { tasks: 1 }]);

  const ask = await computer.submitTurn({ requestId: 'b_2', session: 'current', prompt: 'what is 17*3' });
  await until(() => finished(ask.turnId) === 'completed');
  assert.equal(journal.turnSummary(ask.turnId)?.dispatched, true);
  assert.deepEqual(computer.status().background, { tasks: 1 }, 'answered while the background work still runs');

  await until(() => (computer.status().background as { tasks: number }).tasks === 0 && computer.status().phase === 'ready' && journal.after(0, null, 200).filter((e) => e.type === 'turn.finished').length === 3);
  const evs = journal.after(0, null, 200);
  const bg = evs.find((e) => e.type === 'turn.started' && e.data.reason === 'background');
  assert.ok(bg?.turn);
  assert.equal(bg.session, launch.sessionId);
  assert.equal(finished(bg.turn), 'completed');
  assert.deepEqual([turnText(launch.turnId), turnText(ask.turnId), turnText(bg.turn)], ['launched', 'pong', 'task task_1 finished'], 'each turn has only its own output');
  assert.deepEqual(evs.filter((e) => e.type.startsWith('task.')).map((e) => [e.type, e.turn]), [['task.started', launch.turnId], ['task.finished', null]]);
  await computer.shutdown(1000);
  journal.close();
});
