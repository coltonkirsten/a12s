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

test('a credential file a harness rewrote on the home drive goes back to tmpfs after the turn', async () => {
  const { lstatSync, readFileSync } = await import('node:fs');
  const { Computer } = await import('../computer.js');
  const { Journal } = await import('../journal.js');
  const { ClaudeAdapter } = await import('../adapters/claude.js');
  const { fixtures, until } = await import('./helpers.js');
  const cfg = testConfig({ credentials: ['anthropic'] });
  const secret = join(cfg.secretsDir, 'anthropic');
  const link = join(cfg.claudeConfigDir, '.credentials.json');
  writeFileSync(secret, '{"claudeAiOauth":{"accessToken":"original"}}', { mode: 0o600 });
  const journal = new Journal(join(cfg.stateDir, 'journal.db'), cfg.computer);
  const adapter = new ClaudeAdapter({ cfg, credentialEnv: () => ({ FAKE_MODE: 'refresh' }), mcpServers: {} }, join(fixtures, 'fake-claude.js'));
  const computer = new Computer(cfg, journal, adapter, {});
  await computer.start();
  assert.ok(lstatSync(link).isSymbolicLink());
  const t = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'hi' });
  await until(() => journal.turnInfo(t.turnId)?.finished === 'completed');
  assert.ok(lstatSync(link).isSymbolicLink(), 'symlink restored');
  assert.equal(readlinkSync(link), secret);
  assert.match(readFileSync(secret, 'utf8'), /refreshed/, 'fresh token kept, on tmpfs');
  assert.equal(lstatSync(secret).mode & 0o777, 0o600);
  const diag = journal.after(0, t.turnId, 100).find((e) => e.type === 'diagnostic');
  assert.deepEqual(diag?.data, { message: 'refreshed credential moved back onto tmpfs', names: ['anthropic'] });
  journal.close();
});
