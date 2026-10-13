import assert from 'node:assert/strict';
import { join } from 'node:path';
import { test } from 'node:test';
import { Journal } from '../journal.js';
import { testConfig } from './helpers.js';

test('journal appends with increasing seq, filters by turn, and survives reopen', () => {
  const cfg = testConfig();
  const path = join(cfg.stateDir, 'journal.db');
  const j = new Journal(path, 'scout');
  const seen: number[] = [];
  j.subscribe((e) => seen.push(e.seq));
  const a = j.append('lifecycle', { phase: 'ready' });
  const b = j.acceptTurn('r1', 't_1', 'h1', { resumed: false }, 's1');
  const c = j.append('text.delta', { text: 'hi' }, { turn: 't_1', session: 's1' });
  j.append('turn.finished', { status: 'completed', usage: null }, { turn: 't_1', session: 's1' });
  assert.deepEqual([a.seq, b.seq, c.seq], [1, 2, 3]);
  assert.deepEqual(seen, [1, 2, 3, 4]);
  assert.deepEqual(b, { v: 1, agentComputer: 'scout', computer: 'scout', session: 's1', turn: 't_1', seq: 2, ts: b.ts, type: 'turn.started', data: { resumed: false } });
  assert.deepEqual(j.after(0, 't_1', 10).map((e) => e.type), ['turn.started', 'text.delta', 'turn.finished']);
  assert.deepEqual(j.after(2, null, 1).map((e) => e.seq), [3]);
  assert.deepEqual(j.findRequest('r1'), { turnId: 't_1', bodyHash: 'h1' });
  assert.deepEqual(j.turnInfo('t_1'), { session: 's1', finished: 'completed' });
  j.close();

  const again = new Journal(path, 'scout');
  assert.equal(again.lastSeq(), 4);
  assert.equal(again.append('lifecycle', {}).seq, 5);
  assert.throws(() => again.acceptTurn('r1', 't_2', 'h2', {}, null), /UNIQUE/);
  assert.equal(again.lastSeq(), 5, 'a failed acceptTurn writes nothing');
  again.close();
});

test('inbox.discarded is journaled once per Message id', async () => {
  const { Journal } = await import('../journal.js');
  const { mkdtempSync } = await import('node:fs');
  const { tmpdir } = await import('node:os');
  const { join } = await import('node:path');
  const j = new Journal(join(mkdtempSync(join(tmpdir(), 'j-')), 'journal.db'), 'test');
  const rec = { id: 'm_1', seq: 3, body: 'hello', reason: 'expired' };
  assert.deepEqual(j.journalInbox([rec, { id: 'm_2', seq: 4, body: 'b', reason: 'wake-clear' }]), ['m_1', 'm_2']);
  assert.deepEqual(j.journalInbox([rec]), ['m_1']);
  const evs = j.after(0, null, 100).filter((e) => e.type === 'inbox.discarded');
  assert.equal(evs.length, 2);
  assert.equal(evs[0]?.data.body, 'hello');
});

test('tokensOf normalizes both harnesses', async () => {
  const { tokensOf } = await import('../computer.js');
  assert.deepEqual(tokensOf({ inputTokens: 10, outputTokens: 5, cacheReadInputTokens: 100, cacheCreationInputTokens: 7 }), { input: 10, output: 5, cacheRead: 100, cacheWrite: 7 });
  assert.deepEqual(tokensOf({ thread: {}, last: { inputTokens: 120, cachedInputTokens: 100, outputTokens: 9 } }), { input: 20, output: 9, cacheRead: 100, cacheWrite: 0 });
  assert.equal(tokensOf(null), null);
});
