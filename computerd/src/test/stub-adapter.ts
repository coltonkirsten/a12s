import { EventQueue, ResumeError, type Adapter, type AdapterEvent, type TurnInput, type TurnStatus } from '../adapters/adapter.js';

/** An in-memory harness: turns stay open until finish() or stop(). */
export class StubAdapter implements Adapter {
  readonly kind = 'claude-code' as const;
  readonly sessions = new Set<string>();
  readonly inputs: TurnInput[] = [];
  restarts = 0;
  /** When set, startTurn waits until stop() is called, then rejects. */
  stallStart = false;
  private cancelStart: (() => void) | null = null;
  private queue: EventQueue<AdapterEvent> | null = null;
  private n = 0;

  async startTurn(input: TurnInput) {
    this.inputs.push(input);
    if (this.stallStart) await new Promise<void>((_, reject) => (this.cancelStart = () => reject(new Error('stopped before the turn started'))));
    if (input.sessionId && !this.sessions.has(input.sessionId)) throw new ResumeError(`no session ${input.sessionId}`);
    const sessionId = input.sessionId ?? `s_${++this.n}`;
    this.sessions.add(sessionId);
    const q = new EventQueue<AdapterEvent>();
    this.queue = q;
    q.push({ type: 'text.delta', data: { text: 'po' } });
    return { sessionId, events: q };
  }

  finish(status: TurnStatus = 'completed'): void {
    const q = this.queue;
    this.queue = null;
    q?.push({ type: 'turn.finished', data: { status, usage: null } });
    q?.end();
  }

  async stop(): Promise<void> {
    if (this.cancelStart) return this.cancelStart();
    this.finish('interrupted');
  }

  async precheck(sessionId: string): Promise<string | null> {
    return this.sessions.has(sessionId) ? null : `no session ${sessionId}`;
  }

  /** When set, restart() waits for it. */
  restartGate: Promise<void> | null = null;

  async restart(): Promise<void> {
    this.restarts++;
    await this.restartGate;
  }

  async close(): Promise<void> {}
}
