import { chmodSync, chownSync, lstatSync, mkdirSync, readdirSync } from 'node:fs';
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
 * rw work drives whose mount root is root-owned and empty (a fresh volume) are handed to the agent.
 * Mount roots only; nothing inside is touched. `ownerUid` is 0 in the container (tests override it).
 */
export function drivesToHandOver(cfg: Config, ownerUid = 0): string[] {
  return cfg.drives
    .filter((d) => !d.readOnly)
    .filter((d) => {
      try {
        const st = lstatSync(d.at);
        return st.isDirectory() && !st.isSymbolicLink() && st.uid === ownerUid && readdirSync(d.at).length === 0;
      } catch {
        return false;
      }
    })
    .map((d) => d.at);
}

export function prepareDrives(cfg: Config): string[] {
  if (!cfg.isRoot) return [];
  const paths = drivesToHandOver(cfg);
  for (const p of paths) chownSync(p, AGENT_UID, AGENT_GID);
  return paths;
}
