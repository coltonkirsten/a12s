import { join } from 'node:path';

export type HarnessKind = 'claude-code' | 'codex';

export const AGENT_UID = 1000;
export const AGENT_GID = 1000;

export interface Config {
  computer: string;
  fleet: string;
  generation: string;
  harness: { kind: HarnessKind; version: string; model: string | null };
  imageTier: string;
  bundle: string;
  credentials: string[];
  proxy: string | null;
  /** Computers this one may send to (A12S_SEND, from grants.send). */
  send: string[];
  /** Raw A12S_USES (the endpoints of the Services it uses). */
  uses: string;
  /** Mount points of the attached host folders (A12S_ATTACH). */
  attach: string[];
  /** Mount points of the drives that hold an attached folder's kept paths (A12S_KEEP). */
  keep: string[];
  controller: string | null;
  port: number;
  /** True when running as root inside the container; false in dev on a host. */
  isRoot: boolean;
  homeRoot: string;
  agentHome: string;
  stateDir: string;
  runDir: string;
  secretsDir: string;
  tlsDir: string;
  sockPath: string;
  claudeConfigDir: string;
  codexHome: string;
}

function oneOf<T extends string>(name: string, value: string | undefined, allowed: readonly T[], fallback?: T): T {
  const v = value ?? fallback;
  if (v !== undefined && (allowed as readonly string[]).includes(v)) return v as T;
  throw new Error(`${name} must be one of ${allowed.join('|')}, got ${JSON.stringify(value)}`);
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): Config {
  const isRoot = process.getuid?.() === 0;
  // Path overrides exist only for running the daemon on a dev host (not root).
  const homeRoot = (!isRoot && env.A12S_HOME_ROOT) || '/home';
  const runDir = (!isRoot && env.A12S_RUN_DIR) || '/run/a12s';
  const agentHome = join(homeRoot, 'agent');
  const computer = env.A12S_COMPUTER;
  if (!computer) throw new Error('A12S_COMPUTER is required');
  const kind = oneOf<HarnessKind>('A12S_HARNESS_KIND', env.A12S_HARNESS_KIND, ['claude-code', 'codex']);
  return {
    computer,
    fleet: env.A12S_FLEET ?? '',
    generation: env.A12S_GENERATION ?? '0',
    harness: { kind, version: env.A12S_HARNESS_VERSION ?? 'unknown', model: env.A12S_MODEL || null },
    imageTier: env.A12S_IMAGE_TIER ?? 'slim',
    bundle: env.A12S_BUNDLE ?? `/opt/a12s/bundles/${kind}-${env.A12S_HARNESS_VERSION ?? ''}`,
    credentials: (env.A12S_CREDENTIALS ?? '').split(',').map((s) => s.trim()).filter(Boolean),
    proxy: env.A12S_PROXY || null,
    send: (env.A12S_SEND ?? '').split(',').map((s) => s.trim()).filter(Boolean),
    uses: env.A12S_USES ?? '',
    attach: (env.A12S_ATTACH ?? '').split(',').map((s) => s.trim()).filter(Boolean),
    keep: (env.A12S_KEEP ?? '').split(',').map((s) => s.trim()).filter(Boolean),
    controller: env.A12S_CONTROLLER || null,
    port: Number((!isRoot && env.A12S_PORT) || 7443),
    isRoot,
    homeRoot,
    agentHome,
    stateDir: join(homeRoot, '.a12s'),
    runDir,
    secretsDir: join(runDir, 'secrets'),
    tlsDir: join(runDir, 'tls'),
    sockPath: join(runDir, 'sock', 'computerd.sock'),
    claudeConfigDir: join(agentHome, '.claude'),
    codexHome: join(agentHome, '.codex'),
  };
}
