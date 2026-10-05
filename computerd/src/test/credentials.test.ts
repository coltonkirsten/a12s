import assert from 'node:assert/strict';
import { readlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { credentialGeneration, wireCredentials } from '../credentials.js';
import { testConfig } from './helpers.js';

test('credentials are wired by name; generation is read from .generation', () => {
  const cfg = testConfig({ credentials: ['anthropic', 'openai', 'github', 'npm'] });
  assert.equal(credentialGeneration(cfg), null);
  writeFileSync(join(cfg.secretsDir, 'anthropic'), '{"claudeAiOauth":{}}');
  writeFileSync(join(cfg.secretsDir, 'openai'), 'sk-test\n');
  writeFileSync(join(cfg.secretsDir, 'github'), 'ghp_test\n');
  writeFileSync(join(cfg.secretsDir, '.generation'), '3\n');
  const w = wireCredentials(cfg);
  assert.equal(readlinkSync(join(cfg.claudeConfigDir, '.credentials.json')), join(cfg.secretsDir, 'anthropic'));
  assert.deepEqual(w.env, { OPENAI_API_KEY: 'sk-test', GH_TOKEN: 'ghp_test', GITHUB_TOKEN: 'ghp_test' });
  assert.deepEqual(w.missing, ['npm']);
  assert.equal(credentialGeneration(cfg), 3);

  writeFileSync(join(cfg.secretsDir, 'openai'), '{"tokens":{}}');
  assert.deepEqual(wireCredentials(cfg).env.OPENAI_API_KEY, undefined);
  assert.equal(readlinkSync(join(cfg.codexHome, 'auth.json')), join(cfg.secretsDir, 'openai'));
});
