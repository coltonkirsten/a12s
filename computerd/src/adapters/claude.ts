import { randomUUID } from 'node:crypto';
import { open, readdir } from 'node:fs/promises';
import { join } from 'node:path';
import { query, type Options, type Query, type SDKMessage, type SDKUserMessage } from '@anthropic-ai/claude-agent-sdk';
import type { ChildProcessWithoutNullStreams } from 'node:child_process';
import {
  EventQueue,
  ResumeError,
  Tail,
  type Adapter,
  type AdapterEvent,
  type RunningTurn,
  type SessionListener,
  type TaskEvent,
  type TaskStatus,
  type TurnInput,
} from './adapter.js';
import { harnessEnv, spawnAsAgent, type HarnessDeps } from './spawn.js';

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export interface StopTiming {
  termAfterMs: number;
  killAfterMs: number;
}

/** One CLI turn: started by a prompt computerd pushed (`uuid`), or by the CLI itself when background work ended. */
interface CliTurn {
  uuid: string | null;
  out: EventQueue<AdapterEvent>;
  mapper: ClaudeMapper | null;
}

interface Task {
  kind: string;
  description: string;
}

/** One long-lived Claude Code process for one session. */
interface Live {
  query: Query;
  child: ChildProcessWithoutNullStreams | null;
  input: EventQueue<SDKUserMessage>;
  sessionId: string;
  cwd: string;
  resuming: boolean;
  stderr: Tail;
  /** The first init passed the session check; later turns' inits are not re-checked. */
  initSeen: boolean;
  /** The CLI announces queued prompts with command_lifecycle frames; older CLIs do not. */
  lifecycle: boolean;
  /** Pushed, not yet started by the CLI. */
  pending: CliTurn | null;
  current: CliTurn | null;
  tasks: Map<string, Task>;
  foreground: Map<string, Task>;
  /** Being ended on purpose: stop, restart, close or a switch to another session. */
  ending: boolean;
  resumeFailed: string | null;
  timers: NodeJS.Timeout[];
  exited: Promise<void>;
}

/**
 * Claude Code through the Agent SDK in streaming-input mode, pinned to the bundle's binary. One process per session,
 * kept across turns so background sub-agents and shells outlive the turn that started them.
 */
export class ClaudeAdapter implements Adapter {
  readonly kind = 'claude-code' as const;
  private live: Live | null = null;
  private starting: { cancelled: boolean } | null = null;
  private listener: SessionListener | null = null;

  constructor(
    private readonly deps: HarnessDeps,
    private readonly executable = join(deps.cfg.bundle, 'bin', 'claude'),
    private readonly timing: StopTiming = { termAfterMs: 10_000, killAfterMs: 5_000 },
  ) {}

  listen(listener: SessionListener): void {
    this.listener = listener;
  }

  backgroundTasks(): number {
    return this.live?.tasks.size ?? 0;
  }

  async startTurn(input: TurnInput): Promise<RunningTurn> {
    const busy = this.live && (this.live.pending || this.live.current?.uuid);
    if (this.starting || busy) throw new Error('a turn is already running');
    const start = { cancelled: false };
    this.starting = start;
    let live = this.live;
    try {
      if (live && (live.ending || live.sessionId !== input.sessionId || live.cwd !== input.cwd)) {
        this.end(live);
        await live.exited;
        live = null;
      }
      if (!live && input.sessionId) {
        const reason = await this.precheck(input.sessionId);
        if (reason) throw new ResumeError(reason);
      }
    } finally {
      this.starting = null;
    }
    if (start.cancelled) throw new Error('stopped before the turn started');
    live ??= this.spawn(input);
    const uuid = randomUUID();
    const turn: CliTurn = { uuid, out: new EventQueue(), mapper: null };
    live.pending = turn;
    // `later` keeps the prompt queued until a running turn's result instead of folding it into that turn.
    live.input.push({ type: 'user', message: { role: 'user', content: input.prompt }, parent_tool_use_id: null, uuid, priority: 'later' });
    return { sessionId: live.sessionId, events: turn.out };
  }

  private spawn(input: TurnInput): Live {
    const sessionId = input.sessionId ?? randomUUID();
    let exited!: () => void;
    const live: Live = {
      query: null as unknown as Query,
      child: null,
      input: new EventQueue(),
      sessionId,
      cwd: input.cwd,
      resuming: input.sessionId !== null,
      stderr: new Tail(),
      initSeen: false,
      lifecycle: false,
      pending: null,
      current: null,
      tasks: new Map(),
      foreground: new Map(),
      ending: false,
      resumeFailed: null,
      timers: [],
      exited: new Promise((resolve) => (exited = resolve)),
    };
    const options: Options = {
      pathToClaudeCodeExecutable: this.executable,
      spawnClaudeCodeProcess: (o) => {
        const child = spawnAsAgent(this.deps.cfg, o.command, o.args, { cwd: o.cwd ?? input.cwd, env: o.env });
        child.stderr.setEncoding('utf8').on('data', (d: string) => live.stderr.add(d));
        o.signal.addEventListener('abort', () => child.kill('SIGTERM'), { once: true });
        live.child = child;
        return child;
      },
      cwd: input.cwd,
      env: { ...harnessEnv(this.deps), CLAUDE_AGENT_SDK_CLIENT_APP: 'a12s-computerd/0.1.0' },
      ...(input.model ? { model: input.model } : {}),
      ...(input.sessionId ? { resume: input.sessionId } : { sessionId }),
      settingSources: ['user', 'project', 'local'],
      mcpServers: Object.fromEntries(
        Object.entries(input.mcpServers).map(([name, s]) => [name, { type: 'stdio' as const, command: s.command, args: s.args, env: s.env ?? {}, ...(s.alwaysLoad ? { alwaysLoad: true } : {}) }]),
      ),
      strictMcpConfig: false, // agent-written MCP configs on the home drive must load after restart_harness
      // The CLI echoes each user message it took in: the evidence of receipt behind turn.dispatched.
      extraArgs: { 'replay-user-messages': null },
      includePartialMessages: true,
      permissionMode: 'bypassPermissions' as const,
      allowDangerouslySkipPermissions: true,
    };
    // An open input stream keeps the process, and the background work it runs, alive between turns.
    live.query = query({ prompt: live.input, options });
    this.live = live;
    void this.pump(live, exited);
    return live;
  }

  private async pump(live: Live, exited: () => void): Promise<void> {
    let error: Error | null = null;
    try {
      for await (const message of live.query) this.onMessage(live, message);
    } catch (err) {
      error = err as Error;
    }
    for (const t of live.timers) clearTimeout(t);
    if (this.live === live) this.live = null;
    for (const [id, task] of live.tasks) this.emit(live, { type: 'task.finished', data: { id, ...task, status: 'killed' } });
    live.tasks.clear();
    for (const turn of [live.current, live.pending]) {
      if (!turn) continue;
      if (live.resumeFailed && turn.uuid) {
        turn.out.push({ type: 'turn.finished', data: { status: 'failed', usage: null, error: live.resumeFailed } });
      } else {
        // The SDK also throws after an interrupt; only an unexpected exit is worth a diagnostic.
        if (!live.ending) {
          const message = error ? `claude: ${error.message}` : 'claude: exited without a result';
          turn.out.push({ type: 'diagnostic', data: { message, stderr: live.stderr.toString() } });
        }
        turn.out.push({ type: 'turn.finished', data: { status: live.ending ? 'interrupted' : 'failed', usage: null, error: 'no result' } });
      }
      turn.out.end();
    }
    live.current = live.pending = null;
    exited();
  }

  private onMessage(live: Live, m: SDKMessage): void {
    const frame = m as unknown as { type: string; subtype?: string; command_uuid?: string; state?: string };
    if (frame.type === 'command_lifecycle') {
      live.lifecycle = true;
      if (frame.state === 'started' && live.pending && live.pending.uuid === frame.command_uuid && !live.ending) this.open(live, live.pending);
      return;
    }
    if (m.type === 'system') {
      if (m.subtype === 'task_started' || m.subtype === 'task_updated' || m.subtype === 'task_notification') return this.task(live, m);
      if (m.subtype === 'init' && !live.current && !live.ending) {
        // A CLI without lifecycle frames starts a pushed prompt at the next init; one with them announced it already.
        if (live.pending && !live.lifecycle) this.open(live, live.pending);
        else this.open(live, { uuid: null, out: new EventQueue(), mapper: null });
      }
    }
    const turn = live.current;
    // Between turns the CLI streams sub-agent work and status frames that belong to no turn.
    if (!turn?.mapper) return;
    if (m.type === 'system' && m.subtype === 'init') live.initSeen = true;
    for (const ev of turn.mapper.map(m)) {
      if (ev.type === 'resume.failed' && !live.resumeFailed) {
        live.resumeFailed = ev.data.reason;
        turn.out.push(ev);
        this.end(live);
        continue;
      }
      if (ev.type !== 'turn.finished') {
        turn.out.push(ev);
        continue;
      }
      if (live.resumeFailed && turn.uuid) Object.assign(ev.data, { status: 'failed', error: live.resumeFailed });
      else if (live.ending) ev.data.status = 'interrupted';
      else if (ev.data.status === 'failed') turn.out.push({ type: 'diagnostic', data: { message: 'claude: turn failed', stderr: live.stderr.toString() } });
      turn.out.push(ev);
      turn.out.end();
      live.current = null;
      if (live.ending) this.closeInput(live);
    }
  }

  private open(live: Live, turn: CliTurn): void {
    if (live.current && !live.current.uuid) {
      // A CLI that folds a pushed prompt into a turn it started itself: the rest of that turn answers the prompt.
      live.current.out.push({ type: 'turn.finished', data: { status: 'completed', usage: null } });
      live.current.out.end();
    }
    if (live.pending === turn) live.pending = null;
    turn.mapper = new ClaudeMapper(live.sessionId, live.resuming && !live.initSeen, turn.uuid);
    live.current = turn;
    if (!turn.uuid) this.listener?.backgroundTurn({ sessionId: live.sessionId, events: turn.out });
  }

  private task(live: Live, m: SDKMessage & { type: 'system' }): void {
    const t = m as unknown as {
      subtype: string;
      task_id: string;
      task_type?: string;
      description?: string;
      is_backgrounded?: boolean;
      ambient?: boolean;
      skip_transcript?: boolean;
      status?: TaskStatus;
      patch?: { is_backgrounded?: boolean };
    };
    const id = t.task_id;
    const started = (task: Task) => {
      live.foreground.delete(id);
      live.tasks.set(id, task);
      this.emit(live, { type: 'task.started', data: { id, ...task } });
    };
    switch (t.subtype) {
      case 'task_started': {
        if (t.ambient || t.skip_transcript || live.tasks.has(id)) return;
        const task = { kind: t.task_type ?? 'task', description: t.description ?? '' };
        if (t.is_backgrounded) started(task);
        else live.foreground.set(id, task);
        return;
      }
      case 'task_updated': {
        const fg = live.foreground.get(id);
        if (fg && t.patch?.is_backgrounded) started(fg);
        return;
      }
      case 'task_notification': {
        live.foreground.delete(id);
        const task = live.tasks.get(id);
        if (!task) return;
        live.tasks.delete(id);
        this.emit(live, { type: 'task.finished', data: { id, ...task, status: t.status ?? 'completed' } });
      }
    }
  }

  private emit(live: Live, ev: TaskEvent): void {
    if (live.current) live.current.out.push(ev);
    else this.listener?.taskEvent(ev);
  }

  /** Interrupts a running turn first: SIGTERM would leave it half-recorded in the transcript. */
  private end(live: Live): void {
    if (live.ending) return;
    live.ending = true;
    if (!live.current) return this.closeInput(live);
    live.query.interrupt().catch(() => {});
    live.timers.push(
      setTimeout(() => {
        live.child?.kill('SIGTERM');
        live.timers.push(setTimeout(() => live.child?.kill('SIGKILL'), this.timing.killAfterMs));
      }, this.timing.termAfterMs),
    );
  }

  /** End of input makes the CLI stop its background tasks and exit. */
  private closeInput(live: Live): void {
    live.input.end();
    live.timers.push(
      setTimeout(() => {
        live.child?.kill('SIGTERM');
        live.timers.push(setTimeout(() => live.child?.kill('SIGKILL'), this.timing.killAfterMs));
      }, this.timing.killAfterMs),
    );
  }

  async stop(): Promise<void> {
    if (this.starting) this.starting.cancelled = true;
    if (this.live) this.end(this.live);
  }

  /** The session transcript must exist under $CLAUDE_CONFIG_DIR/projects/ and start with a JSON line. */
  async precheck(sessionId: string): Promise<string | null> {
    if (!UUID.test(sessionId)) return `not a Claude session id: ${sessionId}`;
    const root = join(this.deps.cfg.claudeConfigDir, 'projects');
    let dirs: string[];
    try {
      dirs = await readdir(root);
    } catch {
      return `no session transcripts under ${root}`;
    }
    for (const dir of dirs) {
      const file = join(root, dir, `${sessionId}.jsonl`);
      let fh;
      try {
        fh = await open(file, 'r');
      } catch {
        continue;
      }
      try {
        const buf = Buffer.alloc(64 * 1024);
        const { bytesRead } = await fh.read(buf, 0, buf.length, 0);
        const first = buf.subarray(0, bytesRead).toString('utf8').split('\n')[0] ?? '';
        JSON.parse(first);
        return null;
      } catch {
        return `session transcript ${file} is not JSON lines`;
      } finally {
        await fh.close();
      }
    }
    return `session transcript ${sessionId}.jsonl not found under ${root}`;
  }

  /** The next turn starts a new process that resumes the session and reloads settings, skills and MCP. */
  async restart(): Promise<void> {
    const live = this.live;
    if (!live) return;
    this.end(live);
    await live.exited;
  }

  async close(): Promise<void> {
    if (this.starting) this.starting.cancelled = true;
    await this.restart();
  }
}

type Block = { type: string; [k: string]: unknown };

function blocks(content: unknown): Block[] {
  return Array.isArray(content) ? (content.filter((b) => b && typeof b === 'object' && typeof b.type === 'string') as Block[]) : [];
}

/** Maps SDK messages of one CLI turn onto a12s events. */
export class ClaudeMapper {
  private readonly toolNames = new Map<string, string>();
  private readonly denied = new Set<string>();
  /** The init record passed the session check; the user-message echo seen; turn.dispatched emitted. */
  private initOk = false;
  private echoed = false;
  private dispatched = false;

  /** `prompt` is the uuid of the pushed prompt whose echo is the receipt; a turn the CLI started itself has none. */
  constructor(
    private sessionId: string,
    private readonly resuming = false,
    private readonly prompt: string | null = null,
  ) {}

  map(m: SDKMessage): AdapterEvent[] {
    const out: AdapterEvent[] = [];
    switch (m.type) {
      case 'system':
        if (m.subtype === 'init' && m.session_id && m.session_id !== this.sessionId && this.resuming) {
          // A resume must continue the stored session; anything else would run the prompt without its context.
          out.push({ type: 'resume.failed', data: { sessionId: this.sessionId, reason: `resume of ${this.sessionId} started session ${m.session_id}` } });
        } else if (m.subtype === 'init' && m.session_id && m.session_id !== this.sessionId) {
          out.push({ type: 'session.changed', data: { from: this.sessionId, to: m.session_id } });
          this.sessionId = m.session_id;
          this.initOk = true;
        } else if (m.subtype === 'init') {
          this.initOk = true;
        } else if (m.subtype === 'permission_denied') {
          this.denied.add(m.tool_use_id);
          out.push({ type: 'permission', data: { decision: 'deny', kind: 'tool', tool: m.tool_name, detail: { toolUseId: m.tool_use_id, reason: m.decision_reason_type } } });
        }
        break;
      case 'stream_event': {
        const e = m.event as { type: string; delta?: { type: string; text?: string } };
        if (m.parent_tool_use_id === null && e.type === 'content_block_delta' && e.delta?.type === 'text_delta' && e.delta.text) {
          out.push({ type: 'text.delta', data: { text: e.delta.text } });
        }
        break;
      }
      case 'assistant':
        for (const b of blocks(m.message.content)) {
          if (b.type === 'tool_use' || b.type === 'server_tool_use' || b.type === 'mcp_tool_use') {
            const id = String(b.id);
            const name = String(b.name);
            this.toolNames.set(id, name);
            out.push({ type: 'tool.started', data: { id, name, input: b.input } });
          }
        }
        break;
      case 'user':
        if ((m as { isReplay?: boolean }).isReplay === true && m.parent_tool_use_id === null && this.prompt !== null && m.uuid === this.prompt) this.echoed = true;
        for (const b of blocks(m.message.content)) {
          if (b.type === 'tool_result') {
            const id = String(b.tool_use_id);
            out.push({ type: 'tool.finished', data: { id, name: this.toolNames.get(id) ?? 'unknown', isError: b.is_error === true, output: truncate(b.content) } });
          }
        }
        break;
      case 'result': {
        for (const d of m.permission_denials) {
          if (!this.denied.has(d.tool_use_id)) {
            out.push({ type: 'permission', data: { decision: 'deny', kind: 'tool', tool: d.tool_name, detail: { toolUseId: d.tool_use_id } } });
          }
        }
        const ok = m.subtype === 'success' && !m.is_error;
        const usage = {
          inputTokens: m.usage.input_tokens,
          outputTokens: m.usage.output_tokens,
          cacheReadInputTokens: m.usage.cache_read_input_tokens,
          cacheCreationInputTokens: m.usage.cache_creation_input_tokens,
          costUsd: m.total_cost_usd,
          durationMs: m.duration_ms,
          numTurns: m.num_turns,
        };
        const error = ok ? undefined : m.subtype === 'success' ? m.result : m.errors.join('; ') || m.subtype;
        out.push({ type: 'turn.finished', data: { status: ok ? 'completed' : 'failed', usage, ...(error ? { error } : {}) } });
        break;
      }
    }
    // Receipt needs both the echo and an init that passed the identity check, in either order.
    if (this.initOk && this.echoed && !this.dispatched) {
      this.dispatched = true;
      out.unshift({ type: 'turn.dispatched', data: {} });
    }
    return out;
  }
}

function truncate(v: unknown, max = 2000): unknown {
  const s = typeof v === 'string' ? v : JSON.stringify(v ?? null);
  return s.length > max ? `${s.slice(0, max)}…` : v;
}
