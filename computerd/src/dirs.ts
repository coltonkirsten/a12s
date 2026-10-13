import { chmodSync, chownSync, closeSync, constants, fchownSync, fstatSync, lstatSync, mkdirSync, openSync, readFileSync, realpathSync } from 'node:fs';
import { AGENT_GID, AGENT_UID, type Config } from './config.js';
import { agentFs } from './credentials.js';

/** A root-managed directory. Parents must be root-owned (/home, /run/a12s); a symlink here is refused. */
function ensureRootDir(cfg: Config, path: string, mode: number, owner: 'root' | 'agent'): void {
  mkdirSync(path, { recursive: true, mode });
  const st = lstatSync(path);
  if (!st.isDirectory() || st.isSymbolicLink()) throw new Error(`${path} is not a directory`);
  chmodSync(path, mode);
  if (cfg.isRoot) owner === 'agent' ? chownSync(path, AGENT_UID, AGENT_GID) : chownSync(path, 0, 0);
}

export function prepareDirs(cfg: Config): void {
  ensureRootDir(cfg, cfg.agentHome, 0o755, 'agent');
  ensureRootDir(cfg, cfg.stateDir, 0o700, 'root');
  // 0711: the agent traverses /run/a12s to its secrets and the socket, but cannot list or create anything there.
  ensureRootDir(cfg, cfg.runDir, 0o711, 'root');
  ensureRootDir(cfg, cfg.secretsDir, 0o711, 'root');
  ensureRootDir(cfg, cfg.tlsDir, 0o700, 'root');
  ensureRootDir(cfg, `${cfg.runDir}/sock`, 0o755, 'root');
  // Everything below /home/agent is the agent's: created as the agent, never chowned by root.
  for (const r of agentFs(cfg, [{ op: 'mkdir', path: cfg.claudeConfigDir }, { op: 'mkdir', path: cfg.codexHome }])) {
    if (!r.ok) throw new Error(`cannot create harness config dirs: ${r.error}`);
  }
}

/**
 * The kept paths whose drives computerd hands to the agent. Each must be exactly a mount point below the home with
 * no symlink on the way, so a path the agent shaped in an earlier generation can't steer root's chown elsewhere.
 */
export function keepMounts(cfg: Config, mountinfo: string): string[] {
  const mountPoints = new Set(mountinfo.split('\n').map((line) => line.split(' ')[4]).filter(Boolean));
  for (const p of cfg.keep) {
    if (!p.startsWith(`${cfg.agentHome}/`) || !mountPoints.has(p) || realpathSync(p) !== p) {
      throw new Error(`kept path ${p} is not a drive mounted in the home`);
    }
  }
  return cfg.keep;
}

/** A keep drive starts empty and root-owned; the agent installs into it, so it gets the drive's root (only). */
export function prepareKeep(cfg: Config): void {
  if (!cfg.isRoot || cfg.keep.length === 0) return;
  for (const p of keepMounts(cfg, readFileSync('/proc/self/mountinfo', 'utf8'))) {
    const fd = openSync(p, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
    try {
      const st = fstatSync(fd);
      if (st.uid !== AGENT_UID || st.gid !== AGENT_GID) fchownSync(fd, AGENT_UID, AGENT_GID);
    } finally {
      closeSync(fd);
    }
  }
}
