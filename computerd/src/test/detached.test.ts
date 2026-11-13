import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { Detached, LinuxProcesses, MARK, MARK_VAR, tailOf, type DetachedTask } from '../detached.js';
import { testConfig, until } from './helpers.js';

test('a nohup process the harness left behind is found by its mark, with its output file, until it exits', { skip: !LinuxProcesses.available() && 'needs /proc' }, async () => {
  const dir = testConfig().agentHome;
  const out = join(dir, 'nohup.out');
  const detached = new Detached(new LinuxProcesses());
  const env = { PATH: process.env.PATH ?? '/usr/bin:/bin', [MARK_VAR]: MARK };
  // The shell stands in for the harness's Bash tool: it starts the job and exits at once.
  spawnSync('sh', ['-c', `nohup sh -c 'echo started; sleep 1; echo finished' > ${out} 2>&1 &`], { env, cwd: dir });
  const unmarked = spawnSync('sh', ['-c', 'nohup sleep 1 >/dev/null 2>&1 & echo $!'], { encoding: 'utf8' }).stdout.trim();

  const { started } = detached.scan(null);
  assert.equal(started.length, 1, 'only the marked process');
  const task = started[0]!;
  assert.notEqual(String(task.pid), unmarked);
  assert.match(task.command, /echo started/);
  assert.equal(task.output, out);
  assert.equal(detached.size, 1);

  let ended: typeof started = [];
  await until(() => (ended = detached.scan(null).ended).length === 1, 5000);
  assert.equal(ended[0]!.id, task.id);
  assert.equal(detached.size, 0);
  assert.equal(tailOf(out), readFileSync(out, 'utf8'));
  assert.match(tailOf(out)!, /started\nfinished/);
});

test('stop kills the whole detached tree', { skip: !LinuxProcesses.available() && 'needs /proc' }, async () => {
  const detached = new Detached(new LinuxProcesses());
  const env = { PATH: process.env.PATH ?? '/usr/bin:/bin', [MARK_VAR]: MARK };
  spawnSync('sh', ['-c', `setsid sh -c 'sleep 60 & sleep 60; wait' >/dev/null 2>&1 &`], { env });
  let task: DetachedTask | undefined;
  await until(() => {
    detached.scan(null);
    task = [...detached['tasks'].values()][0];
    return (task?.members.size ?? 0) >= 3;
  }, 3000);
  assert.equal(detached.size, 1, 'the shell and both sleeps are one task');
  assert.ok(task && detached.stop(task.id, 1000));
  await until(() => detached.scan(null).ended.length === 1 || detached.size === 0, 5000);
  assert.equal(detached.size, 0);
});
