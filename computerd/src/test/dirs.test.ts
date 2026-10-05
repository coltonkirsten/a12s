import assert from 'node:assert/strict';
import { mkdirSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { loadConfig } from '../config.js';
import { drivesToHandOver, prepareDirs } from '../dirs.js';
import { testConfig } from './helpers.js';

test('A12S_DRIVES parses; absent is fine', () => {
  const base = { A12S_COMPUTER: 'c', A12S_HARNESS_KIND: 'codex' };
  assert.deepEqual(loadConfig(base).drives, []);
  assert.deepEqual(loadConfig({ ...base, A12S_DRIVES: '/work/a:rw,/work/ref:ro' }).drives, [
    { at: '/work/a', readOnly: false },
    { at: '/work/ref', readOnly: true },
  ]);
});

test('only empty, root-owned (here: test-owned), rw mount roots are handed over; symlinks never', () => {
  const cfg = testConfig();
  const at = (n: string) => join(cfg.runDir, n);
  for (const n of ['fresh', 'used', 'ro']) mkdirSync(at(n));
  writeFileSync(join(at('used'), 'x'), '');
  symlinkSync(cfg.stateDir, at('link'));
  cfg.drives = [
    { at: at('fresh'), readOnly: false },
    { at: at('used'), readOnly: false },
    { at: at('ro'), readOnly: true },
    { at: at('link'), readOnly: false },
    { at: at('missing'), readOnly: false },
  ];
  assert.deepEqual(drivesToHandOver(cfg, process.getuid!()), [at('fresh')]);
});

test('prepareDirs: modes per PLAN, and a symlink in place of a root-managed dir is refused', () => {
  const cfg = testConfig();
  prepareDirs(cfg);
  const mode = (p: string) => statSync(p).mode & 0o777;
  assert.deepEqual([mode(cfg.runDir), mode(cfg.secretsDir), mode(cfg.tlsDir), mode(cfg.stateDir)], [0o711, 0o711, 0o700, 0o700]);
  const evil = testConfig();
  symlinkSync(evil.stateDir, join(evil.homeRoot, 'elsewhere'));
  evil.agentHome = join(evil.homeRoot, 'elsewhere');
  assert.throws(() => prepareDirs(evil), /not a directory/);
});
