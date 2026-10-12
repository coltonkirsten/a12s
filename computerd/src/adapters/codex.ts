import { join } from 'node:path';
import type { ChildProcessWithoutNullStreams } from 'node:child_process';
import { createInterface } from 'node:readline';
import { EventQueue, ResumeError, Tail, type Adapter, type AdapterEvent, type McpServer, type RunningTurn, type TurnInput, type TurnStatus } from './adapter.js';
import { harnessEnv, spawnAsAgent, terminate, type HarnessDeps } from './spawn.js';
import type { StopTiming } from './claude.js';

// Codex protocol payloads are large and evolving; the adapter reads only a few fields from them.
type Json = Record<string, any>;

interface ActiveTurn {
  threadId: string;
  turnId: string | null;
  out: EventQueue<AdapterEvent>;
  usage: Record<string, unknown> | null;
  stopping: boolean;
  timer: NodeJS.Timeout | null;
  /** turn.dispatched was emitted: the turn/start response or the first turn notification came in. */
  dispatched: boolean;
}

/** `codex app-server -c mcp_servers.<name>.…` overrides; values are TOML, and JSON strings/arrays are valid TOML. */
export function mcpConfigArgs(servers: Record<string, McpServer>): string[] {
  const args: string[] = [];
  for (const [name, s] of Object.entries(servers)) {
    args.push('-c', `mcp_servers.${name}.command=${JSON.stringify(s.command)}`);
    args.push('-c', `mcp_servers.${name}.args=${JSON.stringify(s.args)}`);
    if (s.cwd) args.push('-c', `mcp_servers.${name}.cwd=${JSON.stringify(s.cwd)}`);
    for (const [k, v] of Object.entries(s.env ?? {})) args.push('-c', `mcp_servers.${name}.env.${k}=${JSON.stringify(v)}`);
  }
  return args;
}

/** Any turn notification from the harness is evidence it received the prompt; the first one, or the turn/start
 * response, emits turn.dispatched ahead of the events. */
function received(t: ActiveTurn): void {
  if (t.dispatched) return;
  t.dispatched = true;
  t.out.push({ type: 'turn.dispatched', data: {} });
}

/** Codex through one long-lived `codex app-server` child speaking newline-delimited JSON-RPC on stdio. */
export class CodexAdapter implements Adapter {
  readonly kind = 'codex' as const;
  private child: ChildProcessWithoutNullStreams | null = null;
  private ready: Promise<void> | null = null;
  private mcpServers: Record<string, McpServer>;
  private nextId = 1;
  private readonly pending = new Map<number, { resolve: (v: Json) => void; reject: (e: Error) => void }>();
  private readonly loaded = new Set<string>();
  private turn: ActiveTurn | null = null;
  private starting: { cancelled: boolean } | null = null;
  private stderr = new Tail();

  constructor(
    private readonly deps: HarnessDeps,
    private readonly executable = join(deps.cfg.bundle, 'bin', 'codex'),
    private readonly timing: StopTiming = { termAfterMs: 10_000, killAfterMs: 5_000 },
    private readonly requestTimeoutMs = 60_000,
  ) {
    this.mcpServers = deps.mcpServers;
  }

  async startTurn(input: TurnInput): Promise<RunningTurn> {
    if (this.turn || this.starting) throw new Error('a turn is already running');
    const start = { cancelled: false };
    this.starting = start;
    try {
      const run = await this.dispatch(input, start);
      if (start.cancelled) throw new Error('stopped before the turn started');
      return run;
    } catch (err) {
      throw start.cancelled ? new Error('stopped before the turn started') : err;
    } finally {
      this.starting = null;
    }
  }

  private async dispatch(input: TurnInput, start: { cancelled: boolean }): Promise<RunningTurn> {
    if (JSON.stringify(input.mcpServers) !== JSON.stringify(this.mcpServers)) {
      this.mcpServers = input.mcpServers;
      await this.restart();
    }
    await this.ensure();
    const policy = this.policy();
    let threadId: string;
    if (input.sessionId === null) {
      const r = await this.request('thread/start', { cwd: input.cwd, ...(input.model ? { model: input.model } : {}), ...policy });
      threadId = String(r.thread.id);
      this.loaded.add(threadId);
    } else {
      threadId = input.sessionId;
      if (!this.loaded.has(threadId)) {
        const reason = await this.resume(threadId, input.cwd, input.model);
        if (reason) throw new ResumeError(reason);
      }
    }
    const turn: ActiveTurn = { threadId, turnId: null, out: new EventQueue(), usage: null, stopping: false, timer: null, dispatched: false };
    this.turn = turn;
    try {
      // The model goes on every turn: a thread loaded by the wake precheck was resumed without it.
      const r = await this.request('turn/start', {
        threadId,
        input: [{ type: 'text', text: input.prompt, text_elements: [] }],
        cwd: input.cwd,
        ...(input.model ? { model: input.model } : {}),
      });
      turn.turnId = String(r.turn.id);
      // The turn/start response carrying the harness's turn id is Codex's evidence of receipt.
      received(turn);
    } catch (err) {
      this.turn = null;
      throw err;
    }
    if (start.cancelled) {
      turn.stopping = true;
      void this.interrupt(turn);
    }
    return { sessionId: threadId, events: turn.out };
  }

  async stop(): Promise<void> {
    if (this.starting) {
      // Mid-startup there is no turn to interrupt; kill the app-server so a stalled request cannot hold the Computer.
      this.starting.cancelled = true;
      if (this.child) void terminate(this.child, this.timing.killAfterMs);
      return;
    }
    const t = this.turn;
    if (!t || t.stopping) return;
    t.stopping = true;
    if (t.turnId) void this.interrupt(t);
  }

  private async interrupt(t: ActiveTurn): Promise<void> {
    // If the interrupt is not honoured in time, kill the app-server; its exit finishes the turn as interrupted.
    t.timer = setTimeout(() => {
      if (this.child) void terminate(this.child, this.timing.killAfterMs);
    }, this.timing.termAfterMs);
    await this.request('turn/interrupt', { threadId: t.threadId, turnId: t.turnId }).catch(() => {});
  }

  async precheck(sessionId: string): Promise<string | null> {
    await this.ensure();
    return this.resume(sessionId, null, null);
  }

  async restart(): Promise<void> {
    if (this.child) await terminate(this.child, this.timing.killAfterMs);
    this.child = null;
    this.ready = null;
    await this.ensure();
  }

  async close(): Promise<void> {
    await this.stop();
    if (this.child) await terminate(this.child, this.timing.killAfterMs);
  }

  private async resume(threadId: string, cwd: string | null, model: string | null): Promise<string | null> {
    try {
      await this.request('thread/resume', { threadId, ...(cwd ? { cwd } : {}), ...(model ? { model } : {}), ...this.policy() });
      this.loaded.add(threadId);
      return null;
    } catch (err) {
      return (err as Error).message;
    }
  }

  private policy(): Json {
    return this.deps.cfg.autonomy === 'full'
      ? { approvalPolicy: 'never', sandbox: 'danger-full-access' }
      : { approvalPolicy: 'on-request', sandbox: 'workspace-write' };
  }

  private ensure(): Promise<void> {
    if (!this.ready) this.ready = this.spawn().catch((err) => {
      this.ready = null;
      throw err;
    });
    return this.ready;
  }

  private async spawn(): Promise<void> {
    this.stderr = new Tail();
    const child = spawnAsAgent(this.deps.cfg, this.executable, [...mcpConfigArgs(this.mcpServers), 'app-server'], {
      cwd: this.deps.cfg.agentHome,
      env: harnessEnv(this.deps),
    });
    this.child = child;
    child.stderr.setEncoding('utf8').on('data', (d: string) => this.stderr.add(d));
    child.stdin.on('error', () => {});
    createInterface({ input: child.stdout }).on('line', (line) => this.onLine(line));
    child.once('error', (err) => this.onExit(child, `spawn failed: ${err.message}`));
    child.once('exit', (code, signal) => this.onExit(child, `exited (code ${code}, signal ${signal})`));
    await this.request('initialize', { clientInfo: { name: 'a12s', title: 'a12s computerd', version: '0.1.0' } });
    this.send({ method: 'initialized' });
  }

  private onExit(child: ChildProcessWithoutNullStreams, why: string): void {
    if (this.child !== child) return;
    this.child = null;
    this.ready = null;
    this.loaded.clear();
    for (const p of this.pending.values()) p.reject(new Error(`codex app-server ${why}`));
    this.pending.clear();
    const t = this.turn;
    if (t) {
      if (!t.stopping) t.out.push({ type: 'diagnostic', data: { message: `codex app-server ${why}`, stderr: this.stderr.toString() } });
      this.finish(t, t.stopping ? 'interrupted' : 'failed', t.stopping ? undefined : `codex app-server ${why}`);
    }
  }

  private finish(t: ActiveTurn, status: TurnStatus, error?: string): void {
    if (t.timer) clearTimeout(t.timer);
    t.out.push({ type: 'turn.finished', data: { status, usage: t.usage, ...(error ? { error } : {}) } });
    t.out.end();
    if (this.turn === t) this.turn = null;
  }

  private request(method: string, params: Json): Promise<Json> {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      if (!this.child) return reject(new Error('codex app-server is not running'));
      const child = this.child;
      const timer = setTimeout(() => {
        if (!this.pending.delete(id)) return;
        reject(new Error(`codex app-server did not answer ${method} within ${this.requestTimeoutMs}ms`));
        void terminate(child, this.timing.killAfterMs);
      }, this.requestTimeoutMs);
      const done = <T>(fn: (v: T) => void) => (v: T) => (clearTimeout(timer), fn(v));
      this.pending.set(id, { resolve: done(resolve), reject: done(reject) });
      this.send({ id, method, params });
    });
  }

  private send(msg: Json): void {
    this.child?.stdin.write(`${JSON.stringify(msg)}\n`);
  }

  private onLine(line: string): void {
    let msg: Json;
    try {
      msg = JSON.parse(line) as Json;
    } catch {
      return;
    }
    if (typeof msg.method === 'string' && msg.id !== undefined) return this.answer(msg);
    if (typeof msg.method === 'string') return this.notify(msg.method, msg.params ?? {});
    const p = this.pending.get(msg.id);
    if (!p) return;
    this.pending.delete(msg.id);
    if (msg.error) p.reject(new Error(String(msg.error.message ?? JSON.stringify(msg.error))));
    else p.resolve(msg.result ?? {});
  }

  /** Every server request gets an answer, or the turn hangs. Nobody is there to approve, so policy decides. */
  private answer(msg: Json): void {
    const p: Json = msg.params ?? {};
    const full = this.deps.cfg.autonomy === 'full';
    let result: Json | null = null;
    let kind = '';
    switch (msg.method) {
      case 'item/commandExecution/requestApproval':
        result = { decision: full ? 'accept' : 'decline' };
        kind = 'commandExecution';
        break;
      case 'item/fileChange/requestApproval':
        result = { decision: full ? 'accept' : 'decline' };
        kind = 'fileChange';
        break;
      case 'item/permissions/requestApproval':
        result = full ? { permissions: p.permissions ?? {}, scope: 'turn' } : { permissions: {} };
        kind = 'permissions';
        break;
      case 'execCommandApproval':
      case 'applyPatchApproval':
        result = { decision: full ? 'approved' : 'denied' };
        kind = msg.method;
        break;
      case 'item/tool/requestUserInput':
        result = { answers: {} };
        kind = 'userInput';
        break;
      case 'mcpServer/elicitation/request':
        result = { action: 'decline' };
        kind = 'elicitation';
        break;
    }
    if (result) {
      this.send({ id: msg.id, result });
      const decision = 'decision' in result ? (['accept', 'approved'].includes(result.decision) ? 'allow' : 'deny') : kind === 'permissions' && full ? 'allow' : 'deny';
      const fields = ['command', 'cwd', 'reason', 'serverName', 'questions', 'permissions'] as const;
      const detail = Object.fromEntries(fields.filter((k) => p[k] !== undefined && p[k] !== null).map((k) => [k, p[k]]));
      if (this.turn) received(this.turn);
      this.turn?.out.push({ type: 'permission', data: { decision, kind, detail } });
    } else {
      this.send({ id: msg.id, error: { code: -32601, message: `a12s computerd does not handle ${msg.method}` } });
      this.turn?.out.push({ type: 'diagnostic', data: { message: `codex: declined server request ${msg.method}` } });
    }
  }

  private notify(method: string, p: Json): void {
    const t = this.turn;
    if (!t || (p.threadId !== undefined && p.threadId !== t.threadId)) return;
    switch (method) {
      case 'item/agentMessage/delta':
        received(t);
        if (p.delta) t.out.push({ type: 'text.delta', data: { text: String(p.delta) } });
        break;
      case 'item/started': {
        const tool = toolOf(p.item);
        received(t);
        if (tool) t.out.push({ type: 'tool.started', data: { id: String(p.item.id), name: tool.name, input: tool.input } });
        break;
      }
      case 'item/completed': {
        const tool = toolOf(p.item);
        received(t);
        if (tool) t.out.push({ type: 'tool.finished', data: { id: String(p.item.id), name: tool.name, isError: tool.isError, output: tool.output } });
        break;
      }
      case 'thread/tokenUsage/updated':
        t.usage = { thread: p.tokenUsage?.total ?? null, last: p.tokenUsage?.last ?? null };
        break;
      case 'error':
        t.out.push({ type: 'diagnostic', data: { message: `codex: ${p.error?.message ?? 'error'}`, willRetry: p.willRetry === true } });
        break;
      case 'turn/completed': {
        if (t.turnId && p.turn?.id !== t.turnId) break;
        const s = String(p.turn?.status);
        const status: TurnStatus = t.stopping || s === 'interrupted' ? 'interrupted' : s === 'completed' ? 'completed' : 'failed';
        this.finish(t, status, p.turn?.error?.message);
        break;
      }
    }
  }
}

function toolOf(item: Json | undefined): { name: string; input: unknown; isError: boolean; output: unknown } | null {
  if (!item) return null;
  const failed = item.status === 'failed' || item.status === 'declined';
  switch (item.type) {
    case 'commandExecution':
      return { name: 'shell', input: { command: item.command, cwd: item.cwd }, isError: failed || (item.exitCode ?? 0) !== 0, output: { exitCode: item.exitCode, output: clip(item.aggregatedOutput) } };
    case 'fileChange':
      return { name: 'apply_patch', input: { paths: (item.changes ?? []).map((c: Json) => c.path) }, isError: failed, output: { status: item.status } };
    case 'mcpToolCall':
      return { name: `mcp__${item.server}__${item.tool}`, input: item.arguments, isError: failed || !!item.error, output: clip(JSON.stringify(item.error ?? item.result ?? null)) };
    case 'dynamicToolCall':
      return { name: String(item.tool), input: item.arguments, isError: failed || item.success === false, output: { status: item.status } };
    case 'webSearch':
      return { name: 'web_search', input: { query: item.query }, isError: false, output: null };
    default:
      return null;
  }
}

function clip(s: unknown, max = 2000): unknown {
  return typeof s === 'string' && s.length > max ? `${s.slice(0, max)}…` : s;
}
