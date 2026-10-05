import { randomUUID } from 'node:crypto';
import { open, readdir } from 'node:fs/promises';
import { join } from 'node:path';
import { query, type Options, type Query, type SDKMessage, type SDKUserMessage } from '@anthropic-ai/claude-agent-sdk';
import type { ChildProcessWithoutNullStreams } from 'node:child_process';
import { EventQueue, ResumeError, Tail, type Adapter, type AdapterEvent, type RunningTurn, type TurnInput } from './adapter.js';
import { harnessEnv, spawnAsAgent, type HarnessDeps } from './spawn.js';

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export interface StopTiming {
  termAfterMs: number;
  killAfterMs: number;
}

interface ActiveTurn {
  query: Query;
  child: ChildProcessWithoutNullStreams | null;
  stopping: boolean;
  timers: NodeJS.Timeout[];
}

/** Claude Code through the Agent SDK, pinned to the bundle's binary. One process per turn. */
export class ClaudeAdapter implements Adapter {
  readonly kind = 'claude-code' as const;
  private active: ActiveTurn | null = null;

  constructor(
    private readonly deps: HarnessDeps,
    private readonly executable = join(deps.cfg.bundle, 'bin', 'claude'),
    private readonly timing: StopTiming = { termAfterMs: 10_000, killAfterMs: 5_000 },
  ) {}

  async startTurn(input: TurnInput): Promise<RunningTurn> {
    if (this.active) throw new Error('a turn is already running');
    if (input.sessionId) {
      const reason = await this.precheck(input.sessionId);
      if (reason) throw new ResumeError(reason);
    }
    const sessionId = input.sessionId ?? randomUUID();
    const events = new EventQueue<AdapterEvent>();
    const stderr = new Tail();
    let endInput!: () => void;
    const inputDone = new Promise<void>((resolve) => (endInput = resolve));
    // An open-ended input stream keeps stdin open, which interrupt() needs; ending it lets the process exit.
    async function* prompt(): AsyncGenerator<SDKUserMessage> {
      yield { type: 'user', message: { role: 'user', content: input.prompt }, parent_tool_use_id: null };
      await inputDone;
    }
    const active: ActiveTurn = { query: null as unknown as Query, child: null, stopping: false, timers: [] };
    const options: Options = {
      pathToClaudeCodeExecutable: this.executable,
      spawnClaudeCodeProcess: (o) => {
        const child = spawnAsAgent(this.deps.cfg, o.command, o.args, { cwd: o.cwd ?? input.cwd, env: o.env });
        child.stderr.setEncoding('utf8').on('data', (d: string) => stderr.add(d));
        o.signal.addEventListener('abort', () => child.kill('SIGTERM'), { once: true });
        active.child = child;
        return child;
      },
      cwd: input.cwd,
      env: { ...harnessEnv(this.deps), CLAUDE_AGENT_SDK_CLIENT_APP: 'a12s-computerd/0.1.0' },
      ...(input.model ? { model: input.model } : {}),
      ...(input.sessionId ? { resume: input.sessionId } : { sessionId }),
      settingSources: ['user', 'project', 'local'],
      mcpServers: Object.fromEntries(
        Object.entries(input.mcpServers).map(([name, s]) => [name, { type: 'stdio' as const, command: s.command, args: s.args, env: s.env ?? {} }]),
      ),
      strictMcpConfig: true,
      includePartialMessages: true,
      ...(input.autonomy === 'full'
        ? { permissionMode: 'bypassPermissions' as const, allowDangerouslySkipPermissions: true }
        : { permissionMode: 'dontAsk' as const, permissionPrompts: 'none' as const }),
    };
    active.query = query({ prompt: prompt(), options });
    this.active = active;
    void this.pump(active, sessionId, events, stderr, endInput);
    return { sessionId, events };
  }

  private async pump(active: ActiveTurn, sessionId: string, out: EventQueue<AdapterEvent>, stderr: Tail, endInput: () => void): Promise<void> {
    const mapper = new ClaudeMapper(sessionId);
    let finished: AdapterEvent | null = null;
    let threw = false;
    try {
      for await (const message of active.query) {
        for (const ev of mapper.map(message)) {
          if (ev.type === 'turn.finished') finished = ev;
          else out.push(ev);
        }
        if (message.type === 'result') endInput();
      }
    } catch (err) {
      // The SDK also throws after yielding an error result (an interrupt, say); the result already says it all.
      threw = true;
      if (!finished) out.push({ type: 'diagnostic', data: { message: `claude: ${(err as Error).message}`, stderr: stderr.toString() } });
    } finally {
      endInput();
      for (const t of active.timers) clearTimeout(t);
      this.active = null;
    }
    // turn.finished goes last, after the process exited, so the next turn never overlaps this one.
    if (finished?.type === 'turn.finished') {
      if (active.stopping) finished.data.status = 'interrupted';
      else if (finished.data.status === 'failed') out.push({ type: 'diagnostic', data: { message: 'claude: turn failed', stderr: stderr.toString() } });
      out.push(finished);
    } else {
      if (!threw && !active.stopping) out.push({ type: 'diagnostic', data: { message: 'claude: exited without a result', stderr: stderr.toString() } });
      out.push({ type: 'turn.finished', data: { status: active.stopping ? 'interrupted' : 'failed', usage: null, error: 'no result' } });
    }
    out.end();
  }

  async stop(): Promise<void> {
    const a = this.active;
    if (!a || a.stopping) return;
    a.stopping = true;
    // interrupt() ends the turn cleanly; SIGTERM would leave it half-recorded in the transcript.
    a.query.interrupt().catch(() => {});
    a.timers.push(
      setTimeout(() => {
        a.child?.kill('SIGTERM');
        a.timers.push(setTimeout(() => a.child?.kill('SIGKILL'), this.timing.killAfterMs));
      }, this.timing.termAfterMs),
    );
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

  /** Nothing to restart between turns: every turn starts a fresh process that reloads settings, skills and MCP. */
  async restart(): Promise<void> {}

  async close(): Promise<void> {
    await this.stop();
  }
}

type Block = { type: string; [k: string]: unknown };

function blocks(content: unknown): Block[] {
  return Array.isArray(content) ? (content.filter((b) => b && typeof b === 'object' && typeof b.type === 'string') as Block[]) : [];
}

/** Maps SDK messages of one turn onto a12s events. */
export class ClaudeMapper {
  private readonly toolNames = new Map<string, string>();
  private readonly denied = new Set<string>();

  constructor(private sessionId: string) {}

  map(m: SDKMessage): AdapterEvent[] {
    const out: AdapterEvent[] = [];
    switch (m.type) {
      case 'system':
        if (m.subtype === 'init' && m.session_id && m.session_id !== this.sessionId) {
          out.push({ type: 'session.changed', data: { from: this.sessionId, to: m.session_id } });
          this.sessionId = m.session_id;
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
    return out;
  }
}

function truncate(v: unknown, max = 2000): unknown {
  const s = typeof v === 'string' ? v : JSON.stringify(v ?? null);
  return s.length > max ? `${s.slice(0, max)}…` : v;
}
