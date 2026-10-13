import assert from 'node:assert/strict';
import { mkdirSync, realpathSync, statSync, symlinkSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { homeMounts, prepareDirs } from '../dirs.js';
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

test('homeMounts: only exact mount points below the home, with no symlink on the way', () => {
  const cfg = testConfig();
  cfg.agentHome = realpathSync(cfg.homeRoot) + '/agent';
  const kept = `${cfg.agentHome}/project/node_modules`;
  mkdirSync(kept, { recursive: true });
  const info = (p: string) => `1 2 0:1 / ${p} rw - ext4 /dev/x rw\n`;
  const notMounted = /not mounted at its own path/;
  assert.deepEqual(homeMounts(cfg, [kept], info(kept)), [kept]);
  assert.throws(() => homeMounts(cfg, [kept], info('/elsewhere')), notMounted);
  assert.throws(() => homeMounts(cfg, ['/run/a12s/secrets'], info('/run/a12s/secrets')), notMounted);
  symlinkSync(`${cfg.agentHome}/project`, `${cfg.agentHome}/link`);
  const viaLink = `${cfg.agentHome}/link/node_modules`;
  assert.throws(() => homeMounts(cfg, [viaLink], info(viaLink)), notMounted);
  // An attached folder's path the agent turned into a symlink: Docker mounted the folder at the link's target.
  mkdirSync(cfg.stateDir, { recursive: true });
  symlinkSync(cfg.stateDir, `${cfg.agentHome}/docs`);
  assert.throws(() => homeMounts(cfg, [`${cfg.agentHome}/docs`], info(`${cfg.agentHome}/docs`)), notMounted);
});
