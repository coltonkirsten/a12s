#!/usr/bin/env node
// Commits a reclaimed credential onto the tmpfs secret (review #35). Run under `flock <secrets>/.lock`, the lock the
// Controller's injection takes around its rename, so the compare-and-swap check and the rename are one step against it.
// Usage: secretcommit.js '<JSON {dir, name, tmp, seen}>'  → prints "committed" or "stale".
import { closeSync, constants, fstatSync, lstatSync, openSync, readFileSync, renameSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

export interface SecretStamp {
  ino: number;
  size: number;
  mtimeMs: number;
  generation: number | null;
}

/** Reads a file in a root-owned tmpfs dir, refusing symlinks and anything but a regular file. */
export function readNoFollow(path: string): string | null {
  let fd: number;
  try {
    fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  } catch {
    return null;
  }
  try {
    return fstatSync(fd).isFile() ? readFileSync(fd, 'utf8') : null;
  } finally {
    closeSync(fd);
  }
}

/** The integer the Controller writes last to `.generation`, or null before the first injection. */
export function generationIn(dir: string): number | null {
  const n = Number.parseInt(readNoFollow(join(dir, '.generation'))?.trim() ?? '', 10);
  return Number.isFinite(n) ? n : null;
}

export function stampIn(dir: string, name: string): SecretStamp {
  const st = lstatSync(join(dir, name));
  return { ino: st.ino, size: st.size, mtimeMs: st.mtimeMs, generation: generationIn(dir) };
}

function sameStamp(a: SecretStamp, b: SecretStamp): boolean {
  return a.ino === b.ino && a.size === b.size && a.mtimeMs === b.mtimeMs && a.generation === b.generation;
}

/** Renames tmp over the secret if neither it nor .generation changed since `seen`; otherwise drops tmp. */
export function commitSecret(dir: string, name: string, tmp: string, seen: SecretStamp): boolean {
  if (!sameStamp(stampIn(dir, name), seen)) {
    rmSync(tmp, { force: true });
    return false;
  }
  renameSync(tmp, join(dir, name));
  return true;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const a = JSON.parse(process.argv[2] ?? '{}') as { dir: string; name: string; tmp: string; seen: SecretStamp };
  process.stdout.write(commitSecret(a.dir, a.name, a.tmp, a.seen) ? 'committed' : 'stale');
}
