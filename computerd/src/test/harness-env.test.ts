import assert from 'node:assert/strict';
import { test } from 'node:test';
import { harnessEnv } from '../adapters/spawn.js';
import { loadConfig } from '../config.js';
import { testConfig } from './helpers.js';

test('the substrate adds to the harness env; its bypass hosts add to the proxy bypass', () => {
  const cfg = testConfig({
    proxy: 'http://10.128.0.2:3128',
    harnessEnv: { GCE_METADATA_HOST: '169.254.169.254', NO_PROXY: '169.254.169.254' },
  });
  const env = harnessEnv({ cfg, credentialEnv: () => ({ ANTHROPIC_API_KEY: 'k' }), mcpServers: {} });
  assert.equal(env.GCE_METADATA_HOST, '169.254.169.254');
  assert.equal(env.NO_PROXY, 'localhost,127.0.0.1,::1,169.254.169.254');
  assert.equal(env.no_proxy, 'localhost,127.0.0.1,::1');
  assert.equal(env.HTTPS_PROXY, 'http://10.128.0.2:3128');
  assert.equal(env.ANTHROPIC_API_KEY, 'k');
});

test('A12S_HARNESS_ENV must be a JSON object of strings', () => {
  const base = { A12S_COMPUTER: 'c', A12S_HARNESS_KIND: 'codex' };
  assert.deepEqual(loadConfig({ ...base, A12S_HARNESS_ENV: '{"A":"b"}' }).harnessEnv, { A: 'b' });
  assert.deepEqual(loadConfig(base).harnessEnv, {});
  assert.throws(() => loadConfig({ ...base, A12S_HARNESS_ENV: '{"A":1}' }), /JSON object of strings/);
  assert.throws(() => loadConfig({ ...base, A12S_HARNESS_ENV: '["A"]' }), /JSON object of strings/);
});

test('A12S_HARNESS_SETTINGS must be a JSON object', () => {
  const base = { A12S_COMPUTER: 'c', A12S_HARNESS_KIND: 'claude-code' };
  assert.deepEqual(loadConfig({ ...base, A12S_HARNESS_SETTINGS: '{"fastMode":true,"env":{"A":"b"}}' }).harnessSettings, { fastMode: true, env: { A: 'b' } });
  assert.deepEqual(loadConfig(base).harnessSettings, {});
  assert.throws(() => loadConfig({ ...base, A12S_HARNESS_SETTINGS: '[]' }), /JSON object/);
  assert.throws(() => loadConfig({ ...base, A12S_HARNESS_SETTINGS: 'true' }), /JSON object/);
  assert.throws(() => loadConfig({ ...base, A12S_HARNESS_SETTINGS: '{fastMode' }), SyntaxError);
});
