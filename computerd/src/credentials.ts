import { chownSync, lchownSync, mkdirSync, readdirSync, readFileSync, readlinkSync, renameSync, rmSync, symlinkSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { AGENT_GID, AGENT_UID, type Config } from './config.js';

export interface Wiring {
  /** Credential env for the harness only. Values are secrets: never log them. */
  env: Record<string, string>;
  /** Names present in the secrets dir. */
  present: string[];
  /** Names the Fleet asked for (A12S_CREDENTIALS) that are not present. */
  missing: string[];
}

/** The integer the Controller writes last to `.generation`, or null before the first injection. */
export function credentialGeneration(cfg: Config): number | null {
  try {
    const n = Number.parseInt(readFileSync(join(cfg.secretsDir, '.generation'), 'utf8').trim(), 10);
    return Number.isFinite(n) ? n : null;
  } catch {
    return null;
  }
}

function isJsonObject(text: string): boolean {
  try {
    const v: unknown = JSON.parse(text);
    return typeof v === 'object' && v !== null && !Array.isArray(v);
  } catch {
    return false;
  }
}

/** Points `link` at `target`, replacing whatever is there (atomically, via rename). */
function linkTo(cfg: Config, link: string, target: string): void {
  try {
    if (readlinkSync(link) === target) return;
  } catch {
    // missing, or a regular file: replace it below
  }
  const dir = dirname(link);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  if (cfg.isRoot) chownSync(dir, AGENT_UID, AGENT_GID);
  const tmp = `${link}.a12s-tmp`;
  rmSync(tmp, { force: true });
  symlinkSync(target, tmp);
  if (cfg.isRoot) lchownSync(tmp, AGENT_UID, AGENT_GID);
  renameSync(tmp, link);
}

/**
 * Wires `/run/a12s/secrets/<name>` by name: anthropic and openai JSON files become symlinks in the
 * harness config dirs, raw keys become env, github becomes GH_TOKEN/GITHUB_TOKEN. Other names stay files.
 */
export function wireCredentials(cfg: Config): Wiring {
  const env: Record<string, string> = {};
  let names: string[] = [];
  try {
    names = readdirSync(cfg.secretsDir).filter((n) => !n.startsWith('.'));
  } catch {
    // no secrets dir yet
  }
  for (const name of names) {
    const path = join(cfg.secretsDir, name);
    const read = () => readFileSync(path, 'utf8').trim();
    switch (name) {
      case 'anthropic': {
        const v = read();
        if (isJsonObject(v)) linkTo(cfg, join(cfg.claudeConfigDir, '.credentials.json'), path);
        else env.ANTHROPIC_API_KEY = v;
        break;
      }
      case 'openai': {
        const v = read();
        if (isJsonObject(v)) linkTo(cfg, join(cfg.codexHome, 'auth.json'), path);
        else env.OPENAI_API_KEY = v;
        break;
      }
      case 'github': {
        const v = read();
        env.GH_TOKEN = v;
        env.GITHUB_TOKEN = v;
        break;
      }
    }
  }
  return { env, present: names, missing: cfg.credentials.filter((n) => !names.includes(n)) };
}
