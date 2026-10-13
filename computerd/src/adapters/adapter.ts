import type { HarnessKind } from '../config.js';

export interface McpServer {
  command: string;
  args: string[];
  env?: Record<string, string>;
  /** Codex honours this; Claude starts MCP servers in the turn's cwd (/home/agent by default). */
  cwd?: string;
  /** Claude Code: list its tools up front instead of behind tool search, so the agent never has to look them up. */
  alwaysLoad?: boolean;
}

export interface TurnInput {
  prompt: string;
  /** Existing harness session to resume, or null for a new one. */
  sessionId: string | null;
  cwd: string;
  model: string | null;
  mcpServers: Record<string, McpServer>;
}

export type TurnStatus = 'completed' | 'failed' | 'interrupted';

/** `killed`: the session ended (stop, sleep, restart, a new session) while the task ran. */
export type TaskStatus = 'completed' | 'failed' | 'stopped' | 'killed';

/** Background work (a sub-agent, a shell) the harness keeps running after the turn that started it. */
export type TaskEvent =
  | { type: 'task.started'; data: { id: string; kind: string; description: string } }
  | { type: 'task.finished'; data: { id: string; kind: string; description: string; status: TaskStatus } };

/** What an adapter yields; computerd journals each one under the turn. */
export type AdapterEvent =
  | { type: 'text.delta'; data: { text: string } }
  | { type: 'tool.started'; data: { id: string; name: string; input?: unknown } }
  | { type: 'tool.finished'; data: { id: string; name: string; isError: boolean; output?: unknown } }
  | { type: 'permission'; data: { decision: 'allow' | 'deny'; kind: string; tool?: string; detail?: unknown } }
  | { type: 'diagnostic'; data: { message: string; [k: string]: unknown } }
  | { type: 'session.changed'; data: { from: string | null; to: string } }
  | { type: 'resume.failed'; data: { sessionId: string; reason: string } }
  | { type: 'turn.finished'; data: { status: TurnStatus; usage: Record<string, unknown> | null; error?: string } }
  /**
   * Evidence the harness received the prompt: Claude's echo of the user message after the init
   * record passed the session check, Codex's turn/start response. Emitted at most once, before any model output.
   */
  | { type: 'turn.dispatched'; data: Record<string, never> }
  | TaskEvent;

export interface RunningTurn {
  sessionId: string;
  /** Ends after exactly one turn.finished. */
  events: AsyncIterable<AdapterEvent>;
}

export interface SessionListener {
  /** A turn the harness started by itself, when background work finished. */
  backgroundTurn(run: RunningTurn): void;
  /** A task event while no turn runs. */
  taskEvent(ev: TaskEvent): void;
}

export interface Adapter {
  readonly kind: HarnessKind;
  listen(listener: SessionListener): void;
  /**
   * Starts a turn in the live session, opening one (or resuming the session in a new process) when needed.
   * Rejects with ResumeError if an existing session cannot be reopened.
   */
  startTurn(input: TurnInput): Promise<RunningTurn>;
  /**
   * Interrupts the running turn, whose stream then ends with turn.finished `interrupted`, and ends the live
   * session with its background work. Idempotent.
   */
  stop(): Promise<void>;
  /** Live background tasks in the session. */
  backgroundTasks(): number;
  /** Checks a session can be resumed without running a model turn. Resolves null if fine, else a reason. */
  precheck(sessionId: string): Promise<string | null>;
  /** Ends the live session and its background work; the next turn resumes it in a new process. */
  restart(): Promise<void>;
  close(): Promise<void>;
}

export class ResumeError extends Error {}

/** A minimal single-consumer async queue. */
export class EventQueue<T> implements AsyncIterable<T> {
  private readonly items: T[] = [];
  private waiter: (() => void) | null = null;
  private ended = false;

  push(item: T): void {
    if (this.ended) return;
    this.items.push(item);
    this.wake();
  }

  end(): void {
    this.ended = true;
    this.wake();
  }

  get isEnded(): boolean {
    return this.ended;
  }

  async *[Symbol.asyncIterator](): AsyncIterator<T> {
    for (;;) {
      const item = this.items.shift();
      if (item !== undefined) {
        yield item;
        continue;
      }
      if (this.ended) return;
      await new Promise<void>((resolve) => (this.waiter = resolve));
    }
  }

  private wake(): void {
    const w = this.waiter;
    this.waiter = null;
    w?.();
  }
}

/** Keeps the last `max` characters of a stream for diagnostics. */
export class Tail {
  private text = '';
  constructor(private readonly max = 4000) {}
  add(chunk: string): void {
    this.text = (this.text + chunk).slice(-this.max);
  }
  toString(): string {
    return this.text;
  }
}
