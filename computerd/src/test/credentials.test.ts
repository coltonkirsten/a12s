import assert from 'node:assert/strict';
import { readdirSync, readFileSync, readlinkSync, renameSync, statSync, symlinkSync, utimesSync, writeFileSync } from 'node:fs';
import { spawn, spawnSync } from 'node:child_process';
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
  const adapter = new ClaudeAdapter({ cfg, credentialEnv: () => ({ FAKE_MODE: 'refresh', FAKE_HOLD_MS: '3000' }), mcpServers: {} }, join(fixtures, 'fake-claude.js'));
  const computer = new Computer(cfg, journal, adapter, {});
  await computer.start();
  assert.ok(lstatSync(link).isSymbolicLink());
  const t = await computer.submitTurn({ requestId: 'r1', session: 'new', prompt: 'hi' });
  await until(() => lstatSync(link).isSymbolicLink() && /refreshed/.test(readFileSync(secret, 'utf8')), 2500);
  assert.equal(journal.turnInfo(t.turnId)?.finished, null, 'reclaimed while the turn still runs');
  await until(() => journal.turnInfo(t.turnId)?.finished === 'completed', 5000);
  assert.ok(lstatSync(link).isSymbolicLink(), 'symlink restored');
  assert.equal(readlinkSync(link), secret);
  assert.match(readFileSync(secret, 'utf8'), /refreshed/, 'fresh token kept, on tmpfs');
  assert.equal(lstatSync(secret).mode & 0o777, 0o600);
  const diag = journal.after(0, t.turnId, 100).find((e) => e.type === 'diagnostic');
  assert.deepEqual(diag?.data, { message: 'refreshed credential moved back onto tmpfs', names: ['anthropic'] });
  await computer.shutdown(100);
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
  // Linux timestamps are tick-coarse: two quick writes often share an mtime, and a tie means "injected wins".
  utimesSync(secret, new Date(Date.now() - 60_000), new Date(Date.now() - 60_000));
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
  assert.match(w2.notes[0] ?? '', /no valid credential JSON/);
  assert.deepEqual(w2.failures, []);
  assert.match(readFileSync(secret, 'utf8'), /refreshed/, 'garbage never overwrites the secret');
});

test('an on-disk credential older than the injected one is discarded, not reclaimed', async () => {
  const cfg = testConfig({ credentials: ['openai'] });
  const secret = join(cfg.secretsDir, 'openai');
  const link = join(cfg.codexHome, 'auth.json');
  writeFileSync(link, '{"tokens":"stale, from before the last injection"}');
  utimesSync(link, new Date(Date.now() - 60_000), new Date(Date.now() - 60_000));
  writeFileSync(secret, '{"tokens":"newly injected"}');
  const w = wireCredentials(cfg);
  assert.deepEqual([w.reclaimed, w.failures], [[], []]);
  assert.match(w.notes[0] ?? '', /older than the injected credential/);
  assert.equal(readlinkSync(link), secret);
  assert.match(readFileSync(secret, 'utf8'), /newly injected/);
});

test('a raw anthropic setup-token becomes CLAUDE_CODE_OAUTH_TOKEN; an API key stays ANTHROPIC_API_KEY', () => {
  const cfg = testConfig({ credentials: ['anthropic'] });
  writeFileSync(join(cfg.secretsDir, 'anthropic'), 'sk-ant-oat01-test\n');
  assert.deepEqual(wireCredentials(cfg).env, { CLAUDE_CODE_OAUTH_TOKEN: 'sk-ant-oat01-test' });
  writeFileSync(join(cfg.secretsDir, 'anthropic'), 'sk-ant-api03-test\n');
  assert.deepEqual(wireCredentials(cfg).env, { ANTHROPIC_API_KEY: 'sk-ant-api03-test' });
});

test('an injection that lands between the reclaim check and its commit wins', async () => {
  const cfg = testConfig({ credentials: ['openai'] });
  const secret = join(cfg.secretsDir, 'openai');
  const link = join(cfg.codexHome, 'auth.json');
  writeFileSync(secret, '{"tokens":"injected v1"}');
  utimesSync(secret, new Date(Date.now() - 60_000), new Date(Date.now() - 60_000));
  writeFileSync(link, '{"tokens":"refreshed on disk"}');
  const w = wireCredentials(cfg, {
    beforeCommit: () => {
      writeFileSync(secret, '{"tokens":"injected v2, a longer value"}'); // the Controller's in-place write
      writeFileSync(join(cfg.secretsDir, '.generation'), '2\n');
    },
  });
  assert.deepEqual(w.reclaimed, []);
  assert.match(w.notes.join(), /injected during reclaim/);
  assert.match(readFileSync(secret, 'utf8'), /injected v2/);
  assert.equal(readlinkSync(link), secret);
  assert.deepEqual(readdirSync(cfg.secretsDir).filter((f) => f.includes('a12s-tmp')), [], 'no temp left behind');
});

// #35: the reclaim's check and rename run under the `.lock` flock the Controller's injection takes around its rename.
// Handshake, no timing (#63): the holder takes the lock, waits until the reclaim has staged its temp file (so it has
// already captured what it saw), then injects and releases. Needs flock(1) (Linux).
const hasFlock = spawnSync('flock', ['--version']).status === 0;
test('a reclaim waits for an injection holding the secrets lock', { skip: !hasFlock && 'no flock(1) here' }, async () => {
  const cfg = testConfig({ credentials: ['openai'] });
  const secret = join(cfg.secretsDir, 'openai');
  const link = join(cfg.codexHome, 'auth.json');
  const lock = join(cfg.secretsDir, '.lock');
  writeFileSync(secret, '{"tokens":"injected v1"}');
  utimesSync(secret, new Date(Date.now() - 60_000), new Date(Date.now() - 60_000));
  writeFileSync(link, '{"tokens":"refreshed on disk"}');
  writeFileSync(lock, '', { mode: 0o644 }); // as an older peer could have left it
  const script =
    'echo held; i=0; while [ ! -e "$2.a12s-tmp" ]; do i=$((i+1)); [ $i -lt 3000 ] || exit 3; sleep 0.01; done; ' +
    'printf %s "$1" > "$2.t" && mv -f "$2.t" "$2" && echo 2 > "$3"';
  const holder = spawn('flock', [lock, 'sh', '-c', script, 'sh', '{"tokens":"injected v2"}', secret, join(cfg.secretsDir, '.generation')]);
  const exited = new Promise((resolve) => holder.on('exit', resolve));
  await new Promise<void>((resolve) => holder.stdout.once('data', () => resolve()));
  const w = wireCredentials(cfg); // stages its temp file, then blocks on the lock until the injection is done
  assert.equal(await exited, 0);
  assert.deepEqual(w.reclaimed, []);
  assert.match(w.notes.join(), /injected during reclaim/);
  assert.match(readFileSync(secret, 'utf8'), /injected v2/);
  assert.equal(statSync(lock).mode & 0o777, 0o600, 'the lock is made owner-only');
  assert.deepEqual(readdirSync(cfg.secretsDir).filter((f) => f.includes('a12s-tmp')), [], 'no temp left behind');
});
