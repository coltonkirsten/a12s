import assert from 'node:assert/strict';
import { existsSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import type { ControllerReply } from '../send.js';
import { Skills, skillsRoot, type Skill } from '../skills.js';
import { testConfig } from './helpers.js';

const md = (name: string) => `---\nname: ${name}\ndescription: ${name} steps\n---\nDo it.\n`;

/** A Controller holding a skill set: GET answers 304 for the digest it last sent. */
function controller(initial: Skill[]) {
  const state = { skills: initial, digest: 1, calls: [] as string[] };
  const call = async (method: string, path: string, payload?: unknown, headers: Record<string, string> = {}): Promise<ControllerReply> => {
    state.calls.push(`${method} ${path}`);
    const name = decodeURIComponent(path.split('/').pop()!);
    if (method === 'PUT') {
      state.skills = [...state.skills.filter((s) => s.name !== name), { name, source: 'agent', files: (payload as { files: Record<string, string> }).files }];
      state.digest++;
      return { status: 200, body: {}, etag: null };
    }
    if (method === 'DELETE') {
      state.skills = state.skills.filter((s) => s.name !== name);
      state.digest++;
      return { status: 200, body: {}, etag: null };
    }
    const tag = `sha256:${state.digest}`;
    if (headers['if-none-match'] === `"${tag}"`) return { status: 304, body: '', etag: `"${tag}"` };
    return { status: 200, body: { storeId: 's1', digest: tag, skills: state.skills }, etag: `"${tag}"` };
  };
  return { state, call };
}

test('skills land in the harness folder, follow a harness switch and never lose a hand-made folder', async () => {
  const cfg = testConfig();
  mkdirSync(cfg.stateDir, { recursive: true });
  const ctl = controller([{ name: 'style', source: 'fleet', files: { 'SKILL.md': md('style') } }]);
  const claude = skillsRoot(cfg);
  mkdirSync(join(claude, 'deploy'), { recursive: true });
  writeFileSync(join(claude, 'deploy', 'SKILL.md'), 'hand made');
  mkdirSync(join(claude, 'mine'), { recursive: true });
  writeFileSync(join(claude, 'mine', 'SKILL.md'), 'left alone');

  const skills = new Skills(cfg, ctl.call);
  assert.deepEqual(await skills.sync(), { digest: 'sha256:1', wrote: true });
  assert.equal(readFileSync(join(claude, 'style', 'SKILL.md'), 'utf8'), md('style'));
  assert.deepEqual(await skills.sync(), { digest: 'sha256:1', wrote: false }, 'an unchanged set is a 304');

  const saved = await skills.save('deploy', { 'SKILL.md': md('deploy'), 'bin/run.sh': '#!/bin/sh\necho hi\n' });
  assert.equal(saved.status, 200);
  assert.equal(readFileSync(join(claude, 'deploy', 'SKILL.md'), 'utf8'), md('deploy'));
  assert.ok(statSync(join(claude, 'deploy', 'bin', 'run.sh')).mode & 0o100, 'a #! file is executable');
  const aside = readdirSync(join(cfg.agentHome, '.a12s-replaced-skills'));
  assert.equal(aside.length, 1);
  assert.equal(readFileSync(join(cfg.agentHome, '.a12s-replaced-skills', aside[0]!, 'SKILL.md'), 'utf8'), 'hand made');

  await skills.save('deploy', { 'SKILL.md': md('deploy') });
  assert.ok(!existsSync(join(claude, 'deploy', 'bin')), 'a replaced skill drops files it no longer has');
  await skills.remove('deploy');
  assert.ok(!existsSync(join(claude, 'deploy')));

  const codex = new Skills({ ...cfg, harness: { ...cfg.harness, kind: 'codex' } }, ctl.call);
  await codex.sync();
  assert.equal(readFileSync(join(cfg.agentHome, '.agents', 'skills', 'style', 'SKILL.md'), 'utf8'), md('style'));
  assert.ok(!existsSync(join(claude, 'style')), 'the other harness folder keeps no copy');
  assert.equal(readFileSync(join(claude, 'mine', 'SKILL.md'), 'utf8'), 'left alone');
});

test('a Controller that cannot answer leaves the folder as it is', async () => {
  const cfg = testConfig();
  mkdirSync(cfg.stateDir, { recursive: true });
  const down = new Skills(cfg, async () => {
    throw new Error('connect ECONNREFUSED');
  });
  assert.equal(await down.sync(), null);
  const old = new Skills(cfg, async () => ({ status: 404, body: { code: 'not_found' }, etag: null }));
  assert.equal(await old.sync(), null);
  assert.ok(!existsSync(skillsRoot(cfg)));
});

test('concurrent syncs never set aside folders another one wrote', async () => {
  const cfg = testConfig();
  mkdirSync(cfg.stateDir, { recursive: true });
  const ctl = controller([{ name: 'a', source: 'agent', files: { 'SKILL.md': md('a') } }, { name: 'b', source: 'agent', files: { 'SKILL.md': md('b') } }]);
  const skills = new Skills(cfg, ctl.call);
  await Promise.all([skills.sync(), skills.sync(), skills.save('c', { 'SKILL.md': md('c') })]);
  assert.ok(!existsSync(join(cfg.agentHome, '.a12s-replaced-skills')));
  assert.deepEqual(readdirSync(skillsRoot(cfg)).sort(), ['a', 'b', 'c']);
});
