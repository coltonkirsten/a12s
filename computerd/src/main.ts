import { chmodSync, chownSync, existsSync, mkdirSync, readFileSync, rmSync, statSync } from 'node:fs';
import { createServer as createHttpServer } from 'node:http';
import { createServer as createHttpsServer } from 'node:https';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { Adapter, McpServer } from './adapters/adapter.js';
import { ClaudeAdapter } from './adapters/claude.js';
import { CodexAdapter } from './adapters/codex.js';
import type { HarnessDeps } from './adapters/spawn.js';
import { AGENT_GID, AGENT_UID, loadConfig, type Config } from './config.js';
import { Computer } from './computer.js';
import { wireCredentials } from './credentials.js';
import { Journal } from './journal.js';
import { Leases, type TlsMaterial } from './leases.js';
import { apiHandler, localHandler } from './server.js';

function log(message: string): void {
  process.stderr.write(`computerd: ${message}\n`);
}

function ensureDir(cfg: Config, path: string, mode: number, owner: 'root' | 'agent'): void {
  mkdirSync(path, { recursive: true, mode });
  chmodSync(path, mode);
  if (cfg.isRoot) owner === 'agent' ? chownSync(path, AGENT_UID, AGENT_GID) : chownSync(path, 0, 0);
}

function prepareDirs(cfg: Config): void {
  ensureDir(cfg, cfg.agentHome, 0o755, 'agent');
  ensureDir(cfg, cfg.stateDir, 0o700, 'root');
  // 0711, not 0700: the agent must traverse /run/a12s to reach its own secrets/ and the sock/ socket.
  ensureDir(cfg, cfg.runDir, 0o711, 'root');
  ensureDir(cfg, cfg.secretsDir, 0o700, 'agent');
  ensureDir(cfg, cfg.tlsDir, 0o700, 'root');
  ensureDir(cfg, dirname(cfg.sockPath), 0o755, 'root');
  ensureDir(cfg, cfg.claudeConfigDir, 0o700, 'agent');
  ensureDir(cfg, cfg.codexHome, 0o700, 'agent');
}

async function waitForTls(cfg: Config): Promise<TlsMaterial> {
  const files = ['ca.crt', 'cert.pem', 'key.pem'].map((f) => join(cfg.tlsDir, f));
  let logged = false;
  while (!files.every((f) => existsSync(f) && statSync(f).size > 0)) {
    if (!logged) log(`waiting for TLS material in ${cfg.tlsDir}`);
    logged = true;
    await new Promise((r) => setTimeout(r, 500));
  }
  const [ca, cert, key] = files.map((f) => readFileSync(f)) as [Buffer, Buffer, Buffer];
  return { ca, cert, key };
}

function mcpServers(cfg: Config): Record<string, McpServer> {
  const servers: Record<string, McpServer> = {
    a12s: { command: process.execPath, args: [join(dirname(fileURLToPath(import.meta.url)), 'systemtools.js')], env: { A12S_SOCK: cfg.sockPath } },
  };
  if (cfg.imageTier === 'browser') {
    const args = ['/opt/a12s/playwright-mcp/node_modules/@playwright/mcp/cli.js', '--config', '/opt/a12s/playwright-mcp/config.json'];
    if (cfg.proxy) args.push('--proxy-server', cfg.proxy);
    servers.playwright = { command: process.execPath, args, cwd: cfg.agentHome };
  }
  return servers;
}

async function main(): Promise<void> {
  const cfg = loadConfig();
  log(`starting for ${cfg.computer} (${cfg.harness.kind} ${cfg.harness.version}, ${cfg.autonomy}, root=${cfg.isRoot})`);
  prepareDirs(cfg);
  const journal = new Journal(join(cfg.stateDir, 'journal.db'), cfg.computer);
  const tls = await waitForTls(cfg);

  const servers = mcpServers(cfg);
  const deps: HarnessDeps = { cfg, credentialEnv: () => wireCredentials(cfg).env, mcpServers: servers };
  const adapter: Adapter = cfg.harness.kind === 'codex' ? new CodexAdapter(deps) : new ClaudeAdapter(deps);
  const computer = new Computer(cfg, journal, adapter, servers);
  const leases = new Leases(cfg, tls, (drive, path, why) =>
    journal.append('diagnostic', { message: `lease lost on ${drive}:${path}`, reason: why instanceof Error ? why.message : why }),
  );

  rmSync(cfg.sockPath, { force: true });
  const local = createHttpServer(localHandler(computer, leases));
  await new Promise<void>((r) => local.listen(cfg.sockPath, r));
  chmodSync(cfg.sockPath, 0o666);

  const api = createHttpsServer({ ...tls, requestCert: true, rejectUnauthorized: true }, apiHandler(computer, journal));
  await new Promise<void>((r) => api.listen(cfg.port, r));
  log(`listening on :${cfg.port} (mTLS) and ${cfg.sockPath}`);

  let stopping = false;
  const shutdown = async (signal: string) => {
    if (stopping) return;
    stopping = true;
    log(`${signal}: closing admission and interrupting the active turn`);
    await computer.shutdown();
    leases.close();
    local.close();
    api.close();
    api.closeAllConnections();
    journal.close();
    process.exit(0);
  };
  process.on('SIGTERM', () => void shutdown('SIGTERM'));
  process.on('SIGINT', () => void shutdown('SIGINT'));

  await computer.start();
  log(`ready: ${JSON.stringify(computer.status())}`);
}

main().catch((err: Error) => {
  log(`fatal: ${err.stack ?? err.message}`);
  process.exit(1);
});
