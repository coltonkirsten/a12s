import assert from 'node:assert/strict';
import { statSync, symlinkSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { prepareDirs } from '../dirs.js';
import { testConfig } from './helpers.js';

test('prepareDirs: modes, and a symlink in place of a root-managed dir is refused', () => {
  const cfg = testConfig();
  prepareDirs(cfg);
  const mode = (p: string) => statSync(p).mode & 0o777;
  assert.deepEqual([mode(cfg.runDir), mode(cfg.secretsDir), mode(cfg.tlsDir), mode(cfg.stateDir)], [0o711, 0o711, 0o700, 0o700]);
  const evil = testConfig();
  symlinkSync(evil.stateDir, join(evil.homeRoot, 'elsewhere'));
  evil.agentHome = join(evil.homeRoot, 'elsewhere');
  assert.throws(() => prepareDirs(evil), /not a directory/);
});
