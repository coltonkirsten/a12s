import assert from 'node:assert/strict';
import { readFileSync, readlinkSync, renameSync, symlinkSync, writeFileSync } from 'node:fs';
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

test('only declared names are read, and a symlinked secret is refused', () => {
  const cfg = testConfig({ credentials: ['github', 'anthropic'] });
  writeFileSync(join(cfg.runDir, 'elsewhere'), 'ghp_planted');
  symlinkSync(join(cfg.runDir, 'elsewhere'), join(cfg.secretsDir, 'github'));
  writeFileSync(join(cfg.secretsDir, 'openai'), 'sk-undeclared');
  writeFileSync(join(cfg.secretsDir, 'anthropic'), 'sk-ant-raw');
  const w = wireCredentials(cfg);
  assert.deepEqual(w.env, { ANTHROPIC_API_KEY: 'sk-ant-raw' });
  assert.deepEqual([w.present, w.missing], [['anthropic'], ['github']]);
});

test('wiring never replaces a regular credential file before its content is on tmpfs', () => {
  const cfg = testConfig({ credentials: ['openai'] });
  const secret = join(cfg.secretsDir, 'openai');
  const link = join(cfg.codexHome, 'auth.json');
  writeFileSync(secret, '{"tokens":"old"}');
  writeFileSync(link, '{"tokens":"refreshed"}'); // left by a harness before computerd could reclaim it
  const w = wireCredentials(cfg);
  assert.deepEqual(w.reclaimed, ['openai']);
  assert.equal(readlinkSync(link), secret);
  assert.match(readFileSync(secret, 'utf8'), /refreshed/);
  writeFileSync(join(cfg.codexHome, 'tmp'), 'x');
  writeFileSync(link + '.new', 'not json');
  renameSync(link + '.new', link);
  const w2 = wireCredentials(cfg);
  assert.deepEqual(w2.reclaimed, []);
  assert.match(w2.problems[0] ?? '', /no valid credential JSON/);
  assert.match(readFileSync(secret, 'utf8'), /refreshed/, 'garbage never overwrites the secret');
});
