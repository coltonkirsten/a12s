import { EventQueue, ResumeError, type Adapter, type AdapterEvent, type SessionListener, type TaskEvent, type TurnInput, type TurnStatus } from '../adapters/adapter.js';

/** An in-memory harness: turns stay open until finish() or stop(). */
export class StubAdapter implements Adapter {
  readonly kind = 'claude-code' as const;
  readonly sessions = new Set<string>();
  readonly inputs: TurnInput[] = [];
  restarts = 0;
  /** When set, startTurn waits until stop() is called, then rejects. */
  stallStart = false;
  /** When false, turns never report receipt (the harness died before it saw the prompt). */
  autoDispatch = true;
  private cancelStart: (() => void) | null = null;
  private queue: EventQueue<AdapterEvent> | null = null;
  private n = 0;
  private listener: SessionListener | null = null;
  private lastSession: string | null = null;
  readonly tasks = new Set<string>();
  /** The open turn the harness started by itself. */
  private bgQueue: EventQueue<AdapterEvent> | null = null;
  stops = 0;

  listen(listener: SessionListener): void {
    this.listener = listener;
  }

  backgroundTasks(): number {
    return this.tasks.size;
  }

  /** Background work starts (inside the open turn if any) or ends. */
  task(id: string, running: boolean): void {
    const ev: TaskEvent = running
      ? { type: 'task.started', data: { id, kind: 'local_agent', description: id } }
      : { type: 'task.finished', data: { id, kind: 'local_agent', description: id, status: 'completed' } };
    if (running) this.tasks.add(id);
    else this.tasks.delete(id);
    const q = this.queue ?? this.bgQueue;
    if (q) q.push(ev);
    else this.listener?.taskEvent(ev);
  }

  /** The harness starts a turn by itself, as Claude Code does when background work ends. */
  backgroundTurn(): void {
    const q = new EventQueue<AdapterEvent>();
    this.bgQueue = q;
    this.listener?.backgroundTurn({ sessionId: this.lastSession ?? 's_none', events: q });
    q.push({ type: 'text.delta', data: { text: 'the sub-agent finished' } });
  }

  finishBackground(status: TurnStatus = 'completed'): void {
    const q = this.bgQueue;
    this.bgQueue = null;
    q?.push({ type: 'turn.finished', data: { status, usage: null } });
    q?.end();
  }

  /** Stop and restart end the session: its background work is gone. */
  private endSession(): void {
    for (const id of [...this.tasks]) this.task(id, false);
  }

  async startTurn(input: TurnInput) {
    this.inputs.push(input);
    if (this.stallStart) await new Promise<void>((_, reject) => (this.cancelStart = () => reject(new Error('stopped before the turn started'))));
    if (input.sessionId && !this.sessions.has(input.sessionId)) throw new ResumeError(`no session ${input.sessionId}`);
    const sessionId = input.sessionId ?? `s_${++this.n}`;
    if (sessionId !== this.lastSession) this.endSession();
    this.sessions.add(sessionId);
    this.lastSession = sessionId;
    const q = new EventQueue<AdapterEvent>();
    this.queue = q;
    if (this.autoDispatch) q.push({ type: 'turn.dispatched', data: {} });
    q.push({ type: 'text.delta', data: { text: 'po' } });
    return { sessionId, events: q };
  }

  /** Reports receipt for the open turn (with autoDispatch off). */
  dispatch(): void {
    this.queue?.push({ type: 'turn.dispatched', data: {} });
  }

  finish(status: TurnStatus = 'completed'): void {
    const q = this.queue;
    this.queue = null;
    q?.push({ type: 'turn.finished', data: { status, usage: null } });
    q?.end();
  }

  async stop(): Promise<void> {
    this.stops++;
    if (this.cancelStart) return this.cancelStart();
    this.finish('interrupted');
    this.finishBackground('interrupted');
    this.endSession();
  }

  async precheck(sessionId: string): Promise<string | null> {
    return this.sessions.has(sessionId) ? null : `no session ${sessionId}`;
  }

  /** When set, restart() waits for it. */
  restartGate: Promise<void> | null = null;

  async restart(): Promise<void> {
    this.restarts++;
    this.endSession();
    await this.restartGate;
  }

  async close(): Promise<void> {}
}
