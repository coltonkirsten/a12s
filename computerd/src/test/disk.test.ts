import assert from 'node:assert/strict';
import { test } from 'node:test';
import { homeDisk, isDiskFull, nextLevel } from '../disk.js';

const u = (pct: number) => ({ usedBytes: pct, totalBytes: 100 });

test('disk level: high at 90%, back to normal only under 80%', () => {
  assert.equal(nextLevel(u(89), 'normal'), 'normal');
  assert.equal(nextLevel(u(90), 'normal'), 'high');
  assert.equal(nextLevel(u(85), 'high'), 'high');
  assert.equal(nextLevel(u(79), 'high'), 'normal');
  assert.equal(nextLevel({ usedBytes: 0, totalBytes: 0 }, 'high'), 'high');
});

test('disk full: SQLITE_FULL and ENOSPC, nothing else', () => {
  assert.ok(isDiskFull({ code: 'ERR_SQLITE_ERROR', errcode: 13 }));
  assert.ok(isDiskFull(Object.assign(new Error('write'), { code: 'ENOSPC' })));
  assert.ok(!isDiskFull({ code: 'ERR_SQLITE_ERROR', errcode: 5 }));
  assert.ok(!isDiskFull(new Error('database or disk is full')));
  assert.ok(!isDiskFull(null));
});

test('home disk: the agent view of a real filesystem', () => {
  const d = homeDisk('/');
  assert.ok(d && d.totalBytes > 0 && d.usedBytes >= 0 && d.usedBytes <= d.totalBytes);
  assert.equal(homeDisk('/no/such/path'), null);
});
