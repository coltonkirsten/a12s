import type { Autonomy, HarnessKind } from '../config.js';

export interface McpServer {
  command: string;
  args: string[];
  env?: Record<string, string>;
  /** Codex honours this; Claude starts MCP servers in the turn's cwd (/home/agent by default). */
  cwd?: string;
}

export interface TurnInput {
  prompt: string;
  /** Existing harness session to resume, or null for a new one. */
  sessionId: string | null;
  cwd: string;
  model: string | null;
  autonomy: Autonomy;
  mcpServers: Record<string, McpServer>;
}

export type TurnStatus = 'completed' | 'failed' | 'interrupted';

/** What an adapter yields; computerd journals each one under the turn. */
export type AdapterEvent =
  | { type: 'text.delta'; data: { text: string } }
  | { type: 'tool.started'; data: { id: string; name: string; input?: unknown } }
  | { type: 'tool.finished'; data: { id: string; name: string; isError: boolean; output?: unknown } }
  | { type: 'permission'; data: { decision: 'allow' | 'deny'; kind: string; tool?: string; detail?: unknown } }
  | { type: 'diagnostic'; data: { message: string; [k: string]: unknown } }
  | { type: 'session.changed'; data: { from: string | null; to: string } }
  | { type: 'turn.finished'; data: { status: TurnStatus; usage: Record<string, unknown> | null; error?: string } };

export interface RunningTurn {
  /** The harness session this turn runs in. */
  sessionId: string;
  /** Ends after exactly one turn.finished. */
  events: AsyncIterable<AdapterEvent>;
}

export interface Adapter {
  readonly kind: HarnessKind;
  /** Starts a turn. Rejects with ResumeError if an existing session cannot be reopened. */
  startTurn(input: TurnInput): Promise<RunningTurn>;
  /** Interrupts the active turn; its stream then ends with turn.finished `interrupted`. Idempotent. */
  stop(): Promise<void>;
  /** Checks a session can be resumed without running a model turn. Resolves null if fine, else a reason. */
  precheck(sessionId: string): Promise<string | null>;
  /** Restarts the harness process (between turns). */
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
