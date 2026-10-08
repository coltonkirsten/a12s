import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { Config } from '../config.js';

export const fixtures = join(dirname(fileURLToPath(import.meta.url)), 'fixtures');

const roots: string[] = [];
process.on('exit', () => roots.forEach((r) => rmSync(r, { recursive: true, force: true })));

export function testConfig(overrides: Partial<Config> = {}): Config {
  const root = mkdtempSync(join(tmpdir(), 'computerd-test-'));
  roots.push(root);
  const homeRoot = join(root, 'home');
  const runDir = join(root, 'run');
  const cfg: Config = {
    computer: 'test',
    fleet: 'lab',
    generation: '1',
    harness: { kind: 'claude-code', version: '0.0.0', model: null },
    autonomy: 'full',
    imageTier: 'slim',
    bundle: join(root, 'bundle'),
    credentials: [],
    drives: [],
    proxy: null,
    send: [],
    uses: '',
    controller: null,
    port: 0,
    isRoot: false,
    homeRoot,
    agentHome: join(homeRoot, 'agent'),
    stateDir: join(homeRoot, '.a12s'),
    runDir,
    secretsDir: join(runDir, 'secrets'),
    tlsDir: join(runDir, 'tls'),
    sockPath: join(runDir, 'sock', 'computerd.sock'),
    claudeConfigDir: join(homeRoot, 'agent', '.claude'),
    codexHome: join(homeRoot, 'agent', '.codex'),
    ...overrides,
  };
  for (const d of [cfg.agentHome, cfg.stateDir, cfg.secretsDir, cfg.claudeConfigDir, cfg.codexHome]) mkdirSync(d, { recursive: true });
  return cfg;
}

export async function collect<T>(it: AsyncIterable<T>): Promise<T[]> {
  const out: T[] = [];
  for await (const x of it) out.push(x);
  return out;
}

export function until(cond: () => boolean, timeoutMs = 5000): Promise<void> {
  const start = Date.now();
  return new Promise((resolve, reject) => {
    const tick = () => {
      if (cond()) return resolve();
      if (Date.now() - start > timeoutMs) return reject(new Error('timed out waiting'));
      setTimeout(tick, 10);
    };
    tick();
  });
}
