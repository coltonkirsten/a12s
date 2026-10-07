import { spawnSync } from 'node:child_process';
import { chownSync, closeSync, constants, fchmodSync, fchownSync, fstatSync, openSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { AgentFsOp, AgentFsResult } from './agentfs.js';
import { AGENT_GID, AGENT_UID, type Config } from './config.js';
import { commitSecret, generationIn, readNoFollow, type SecretStamp, stampIn } from './secretcommit.js';

export interface Wiring {
  /** Credential env for the harness only. Values are secrets: never log them. */
  env: Record<string, string>;
  /** Names from A12S_CREDENTIALS that are present as regular files. */
  present: string[];
  /** Names from A12S_CREDENTIALS that are absent (or not regular files). */
  missing: string[];
  /** Names whose harness-rewritten file was moved back onto tmpfs. */
  reclaimed: string[];
  /** Why a declared credential is not wired (no secret values). Non-empty = the generation is not applied. */
  failures: string[];
  /** Worth a diagnostic, but the credential is wired. */
  notes: string[];
}

const AGENTFS = join(dirname(fileURLToPath(import.meta.url)), 'agentfs.js');
const SECRETCOMMIT = join(dirname(fileURLToPath(import.meta.url)), 'secretcommit.js');

/** Runs filesystem ops inside the agent's home as uid/gid 1000 (as the current user in dev). */
export function agentFs(cfg: Config, ops: AgentFsOp[]): AgentFsResult[] {
  if (ops.length === 0) return [];
  const r = spawnSync(process.execPath, [AGENTFS, JSON.stringify(ops)], {
    env: {},
    encoding: 'utf8',
    timeout: 10_000,
    maxBuffer: 4 << 20,
    ...(cfg.isRoot ? { uid: AGENT_UID, gid: AGENT_GID } : {}),
  });
  if (r.status !== 0) throw new Error(`agentfs failed: ${r.error?.message ?? r.stderr.trim()}`);
  return JSON.parse(r.stdout) as AgentFsResult[];
}

/** The integer the Controller writes last to `.generation`, or null before the first injection. */
export function credentialGeneration(cfg: Config): number | null {
  return generationIn(cfg.secretsDir);
}

function isJsonObject(text: string): boolean {
  try {
    const v: unknown = JSON.parse(text);
    return typeof v === 'object' && v !== null && !Array.isArray(v);
  } catch {
    return false;
  }
}

/** Only the names the Fleet declared (A12S_CREDENTIALS) are ever read. */
function readSecrets(cfg: Config): Map<string, string> {
  const out = new Map<string, string>();
  for (const name of cfg.credentials) {
    if (!/^[a-z0-9][a-z0-9_-]*$/i.test(name)) continue;
    const v = readNoFollow(join(cfg.secretsDir, name));
    if (v !== null) out.set(name, v.trim());
  }
  return out;
}

/** Raw keys and tokens for the harness env. */
export function credentialEnv(cfg: Config): Record<string, string> {
  const env: Record<string, string> = {};
  for (const [name, v] of readSecrets(cfg)) {
    // A `claude setup-token` token (sk-ant-oat…) is long-lived and never refreshed: no credential file at all.
    if (name === 'anthropic' && !isJsonObject(v)) env[v.startsWith('sk-ant-oat') ? 'CLAUDE_CODE_OAUTH_TOKEN' : 'ANTHROPIC_API_KEY'] = v;
    if (name === 'openai' && !isJsonObject(v)) env.OPENAI_API_KEY = v;
    if (name === 'github') env.GH_TOKEN = env.GITHUB_TOKEN = v;
  }
  return env;
}

/**
 * Moves reclaimed content onto the tmpfs secret: temp file + rename inside tmpfs (root-owned, so the rename cannot be
 * redirected), so a crash never leaves a half moved secret. The Controller injects from outside this process, so the
 * commit is a compare-and-swap: if the secret or .generation changed since `seen`, the reclaim is dropped and the
 * injected material wins. The check and the rename run under the `.lock` flock the injection's rename also takes
 * (review #35), and the injection stamps its file's mtime at publication under that lock (#61), so a refresh staged
 * before it reads as older; an image without flock(1) falls back to the unlocked check, a microsecond window.
 */
function writeSecret(cfg: Config, name: string, content: string, seen: SecretStamp, beforeCommit?: () => void): boolean {
  const path = join(cfg.secretsDir, name);
  const tmp = `${path}.a12s-tmp`;
  writeFileSync(tmp, content, { mode: 0o600, flag: 'w' });
  if (cfg.isRoot) chownSync(tmp, AGENT_UID, AGENT_GID);
  beforeCommit?.();
  const lock = join(cfg.secretsDir, '.lock');
  secureLock(cfg, lock);
  const args = JSON.stringify({ dir: cfg.secretsDir, name, tmp, seen });
  const r = spawnSync('flock', ['-w', '10', lock, process.execPath, SECRETCOMMIT, args], {
    env: { PATH: process.env.PATH ?? '/usr/bin:/bin' },
    encoding: 'utf8',
    timeout: 15_000,
  });
  if ((r.error as NodeJS.ErrnoException | undefined)?.code === 'ENOENT') return commitSecret(cfg.secretsDir, name, tmp, seen);
  if (r.status === 0 && (r.stdout === 'committed' || r.stdout === 'stale')) return r.stdout === 'committed';
  rmSync(tmp, { force: true });
  throw new Error(`secret commit failed: ${r.error?.message ?? (r.stderr.trim() || `exit ${r.status}`)}`);
}

/**
 * Creates the lock file, or repairs one created by an older peer, as root-only 0600 (review #62): the agent can
 * traverse secrets/, and an agent-readable lock would let it hold the flock and stall reclaim and injection.
 */
function secureLock(cfg: Config, lock: string): void {
  const fd = openSync(lock, constants.O_RDONLY | constants.O_CREAT | constants.O_NOFOLLOW, 0o600);
  try {
    const st = fstatSync(fd);
    if (!st.isFile()) throw new Error(`${lock} is not a regular file`);
    if (cfg.isRoot && (st.uid !== 0 || st.gid !== 0)) fchownSync(fd, 0, 0);
    if ((st.mode & 0o777) !== 0o600) fchmodSync(fd, 0o600);
  } finally {
    closeSync(fd);
  }
}

function stamp(cfg: Config, name: string): SecretStamp {
  return stampIn(cfg.secretsDir, name);
}

/** The credential files harnesses rewrite on refresh, as [dir, file name], for watching. */
export function credentialFiles(cfg: Config): [string, string][] {
  return [
    [cfg.claudeConfigDir, '.credentials.json'],
    [cfg.codexHome, 'auth.json'],
  ];
}

/**
 * Wires credentials by name: anthropic/openai JSON → symlinks in the harness config dirs; raw keys and github → env.
 * A harness that refreshed a subscription token replaced its symlink with a regular file on the home drive: that
 * content is moved onto the tmpfs secret first, then the symlink is restored. Agent-side steps run as the agent.
 */
export function wireCredentials(cfg: Config, hooks: { beforeCommit?: () => void } = {}): Wiring {
  const secrets = readSecrets(cfg);
  const links: [string, string][] = [];
  const [claude, codex] = credentialFiles(cfg);
  for (const [name, link] of [
    ['anthropic', join(...claude!)],
    ['openai', join(...codex!)],
  ] as const) {
    if (secrets.has(name) && isJsonObject(secrets.get(name)!)) links.push([name, link]);
  }
  const reclaimed: string[] = [];
  const failures: string[] = [];
  const notes: string[] = [];
  const toLink: AgentFsOp[] = [];
  agentFs(cfg, links.map(([, path]) => ({ op: 'inspect', path }))).forEach((r, i) => {
    const [name, path] = links[i]!;
    const target = join(cfg.secretsDir, name);
    if (!r.ok) return void failures.push(`${path}: ${r.error}`);
    if (r.kind === 'symlink' && r.target === target) return;
    if (r.kind === 'other') return void failures.push(`${path} is not a file or symlink; left alone`);
    if (r.kind === 'file') {
      // Only a file written after the tmpfs secret is a refresh; an older one would undo a newer injection.
      // Equal mtimes (Linux timestamps are tick-coarse) are ambiguous: the injected secret wins.
      const seen = stamp(cfg, name);
      if ((r.mtimeMs ?? 0) <= seen.mtimeMs) notes.push(`${path} was older than the injected credential; discarded`);
      else if (!r.content || !isJsonObject(r.content)) notes.push(`${path} held no valid credential JSON; replaced by the injected one`);
      else if (writeSecret(cfg, name, r.content, seen, hooks.beforeCommit)) reclaimed.push(name);
      else notes.push(`${path}: a new credential was injected during reclaim; the injected one wins`);
    }
    toLink.push({ op: 'link', path, target });
  });
  agentFs(cfg, toLink).forEach((r, i) => {
    if (!r.ok) failures.push(`${toLink[i]!.path}: ${r.error}`);
  });
  const present = [...secrets.keys()];
  const missing = cfg.credentials.filter((n) => !present.includes(n));
  if (missing.length) failures.push(`not injected: ${missing.join(', ')}`);
  return { env: credentialEnv(cfg), present, missing, reclaimed, failures, notes };
}
