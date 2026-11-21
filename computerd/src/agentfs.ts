#!/usr/bin/env node
// Filesystem steps inside the agent's home, run AS the agent (uid 1000): root must never follow a path the agent controls.
// Usage: agentfs.js < '<JSON list of ops>'  → prints a JSON list of results.
import { chmodSync, closeSync, constants, fstatSync, lstatSync, mkdirSync, openSync, readdirSync, readFileSync, readlinkSync, renameSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';

export type AgentFsOp =
  | { op: 'mkdir'; path: string }
  | { op: 'inspect'; path: string }
  | { op: 'link'; path: string; target: string }
  | { op: 'write'; path: string; content: string; mode: number }
  | { op: 'remove'; path: string }
  | { op: 'rename'; path: string; target: string }
  | { op: 'list'; path: string };
export type AgentFsResult =
  | { ok: true; kind?: 'missing' | 'symlink' | 'file' | 'other'; target?: string; content?: string; mtimeMs?: number; names?: string[] }
  | { ok: false; error: string };

const MAX_FILE = 1 << 20;

function run(op: AgentFsOp): AgentFsResult {
  switch (op.op) {
    case 'mkdir':
      mkdirSync(op.path, { recursive: true, mode: 0o700 });
      return { ok: true };
    case 'inspect': {
      let st;
      try {
        st = lstatSync(op.path);
      } catch {
        return { ok: true, kind: 'missing' };
      }
      if (st.isSymbolicLink()) return { ok: true, kind: 'symlink', target: readlinkSync(op.path) };
      if (!st.isFile()) return { ok: true, kind: 'other' };
      const fd = openSync(op.path, constants.O_RDONLY | constants.O_NOFOLLOW);
      try {
        const fst = fstatSync(fd);
        if (fst.size > MAX_FILE) return { ok: true, kind: 'other' };
        return { ok: true, kind: 'file', content: readFileSync(fd, 'utf8'), mtimeMs: fst.mtimeMs };
      } finally {
        closeSync(fd);
      }
    }
    case 'link': {
      mkdirSync(dirname(op.path), { recursive: true, mode: 0o700 });
      const tmp = `${op.path}.a12s-tmp`;
      rmSync(tmp, { force: true });
      symlinkSync(op.target, tmp);
      renameSync(tmp, op.path); // atomic; replaces a regular file the harness left (its content was reclaimed first)
      return { ok: true };
    }
    case 'write': {
      mkdirSync(dirname(op.path), { recursive: true, mode: 0o755 });
      const tmp = `${op.path}.a12s-tmp`;
      rmSync(tmp, { force: true });
      writeFileSync(tmp, op.content, { mode: op.mode, flag: 'wx' });
      chmodSync(tmp, op.mode);
      renameSync(tmp, op.path);
      return { ok: true };
    }
    case 'remove':
      rmSync(op.path, { recursive: true, force: true });
      return { ok: true };
    case 'rename':
      mkdirSync(dirname(op.target), { recursive: true, mode: 0o700 });
      renameSync(op.path, op.target);
      return { ok: true };
    case 'list':
      try {
        return { ok: true, names: readdirSync(op.path) };
      } catch {
        return { ok: true, names: [] };
      }
  }
}

const ops = JSON.parse(readFileSync(0, 'utf8') || '[]') as AgentFsOp[];
process.stdout.write(
  JSON.stringify(
    ops.map((op) => {
      try {
        return run(op);
      } catch (err) {
        return { ok: false, error: (err as Error).message };
      }
    }),
  ),
);
