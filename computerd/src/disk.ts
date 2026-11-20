import { statfsSync } from 'node:fs';

/** The home drive as the agent sees it: space reserved for root (computerd's own) counts in neither. */
export interface DiskUsage {
  usedBytes: number;
  totalBytes: number;
}

export type DiskLevel = 'high' | 'normal';

/** At HIGH the agent is told to clean up, once; it is told again only after use fell under NORMAL. */
export const HIGH = 0.9;
export const NORMAL = 0.8;

export function homeDisk(path: string): DiskUsage | null {
  try {
    const s = statfsSync(path);
    const total = (s.blocks - (s.bfree - s.bavail)) * s.bsize;
    return { usedBytes: total - s.bavail * s.bsize, totalBytes: total };
  } catch {
    return null;
  }
}

export function nextLevel(u: DiskUsage, prev: DiskLevel): DiskLevel {
  if (u.totalBytes <= 0) return prev;
  const used = u.usedBytes / u.totalBytes;
  if (used >= HIGH) return 'high';
  if (used < NORMAL) return 'normal';
  return prev;
}

/** SQLite's SQLITE_FULL, or a file write that hit ENOSPC. */
export function isDiskFull(err: unknown): boolean {
  if (typeof err !== 'object' || err === null) return false;
  const e = err as { code?: unknown; errcode?: unknown };
  return e.code === 'ENOSPC' || (e.code === 'ERR_SQLITE_ERROR' && typeof e.errcode === 'number' && (e.errcode & 0xff) === 13);
}
