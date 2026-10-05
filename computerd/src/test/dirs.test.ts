import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { loadConfig } from '../config.js';
import { drivesToHandOver, prepareDirs, reservedDrivePath } from '../dirs.js';
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
  // Not os.tmpdir() (/tmp on Linux) nor the checkout (/home/runner on CI): the handover rightly refuses both as
  // reserved. /var/tmp is world-writable on Linux and macOS.
  const work = mkdtempSync('/var/tmp/computerd-test-drives-');
  const at = (n: string) => join(work, n);
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
  rmSync(work, { recursive: true, force: true });
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

test('reserved or ambiguous drive paths are never handed over', () => {
  const cfg = testConfig();
  for (const bad of ['/home/.a12s', '/home', '/run/a12s/secrets', '/opt/a12s/bundles', '/tmp/x', 'work/rel', '/work/../home/.a12s', '/work/a/', '/', join(cfg.homeRoot, '.a12s')]) {
    assert.ok(reservedDrivePath(cfg, bad), bad);
  }
  assert.equal(reservedDrivePath(cfg, '/work/a12s'), null);
  // A comma-injected list entry parses into its own path, and that path is refused.
  const injected = loadConfig({ A12S_COMPUTER: 'c', A12S_HARNESS_KIND: 'codex', A12S_DRIVES: '/work/volume,/home/.a12s:rw' });
  const logs: string[] = [];
  assert.deepEqual(drivesToHandOver({ ...cfg, drives: injected.drives }, 0, (m) => logs.push(m)), []);
  assert.match(logs.join('\n'), /"\/home\/.a12s" skipped: inside reserved \/home/);
});
