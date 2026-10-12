import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { AGENT_GID, AGENT_UID, type Config } from '../config.js';
import type { McpServer } from './adapter.js';

export interface HarnessDeps {
  cfg: Config;
  /** Current credential env (API keys, tokens); read at every spawn so rotation applies. */
  credentialEnv: () => Record<string, string>;
  /** MCP servers a long-lived harness (Codex) is spawned with before its first turn. */
  mcpServers: Record<string, McpServer>;
  /** Service wiring: A12S_SERVICE_<SVC>_<PORT> addresses. */
  wiringEnv?: Record<string, string>;
}

/** The whole environment a harness process sees. Nothing from computerd's own env leaks except PATH and locale. */
export function harnessEnv(deps: HarnessDeps): Record<string, string> {
  const { cfg } = deps;
  const env: Record<string, string> = {
    PATH: process.env.PATH ?? '/usr/local/bin:/usr/bin:/bin',
    HOME: cfg.agentHome,
    USER: 'agent',
    LANG: process.env.LANG ?? 'C.UTF-8',
    CLAUDE_CONFIG_DIR: cfg.claudeConfigDir,
    CODEX_HOME: cfg.codexHome,
  };
  if (!cfg.isRoot && process.env.TMPDIR) env.TMPDIR = process.env.TMPDIR;
  // The browser tier ships Playwright here, so agent scripts can require("playwright").
  if (cfg.imageTier === 'browser') env.NODE_PATH = '/opt/a12s/playwright-mcp/node_modules';
  if (cfg.proxy) {
    for (const k of ['HTTPS_PROXY', 'HTTP_PROXY', 'https_proxy', 'http_proxy']) env[k] = cfg.proxy;
    env.NO_PROXY = env.no_proxy = 'localhost,127.0.0.1,::1';
  }
  return { ...env, ...(deps.wiringEnv ?? {}), ...deps.credentialEnv() };
}

/** Spawns a harness process as the agent user (uid/gid 1000) when computerd is root. */
export function spawnAsAgent(
  cfg: Config,
  command: string,
  args: string[],
  opts: { cwd: string; env: Record<string, string | undefined> },
): ChildProcessWithoutNullStreams {
  return spawn(command, args, {
    cwd: opts.cwd,
    env: opts.env,
    stdio: ['pipe', 'pipe', 'pipe'],
    ...(cfg.isRoot ? { uid: AGENT_UID, gid: AGENT_GID } : {}),
  });
}

/** SIGTERM, then SIGKILL after `graceMs`. Resolves when the process has exited. */
export function terminate(child: ChildProcessWithoutNullStreams, graceMs: number): Promise<void> {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(() => child.kill('SIGKILL'), graceMs);
    child.once('exit', () => {
      clearTimeout(timer);
      resolve();
    });
    child.kill('SIGTERM');
  });
}
