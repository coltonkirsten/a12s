import { createHash, randomBytes } from 'node:crypto';
import { chownSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { ResumeError, type Adapter, type AdapterEvent, type McpServer, type TurnStatus } from './adapters/adapter.js';
import type { Config, HarnessKind } from './config.js';
import { credentialGeneration, wireCredentials } from './credentials.js';
import type { Journal } from './journal.js';

export interface SessionRecord {
  sessionId: string;
  harness: HarnessKind;
  cwd: string;
}

export interface TurnRequest {
  requestId: string;
  session: string;
  prompt: string;
  cwd?: string;
}

export interface TurnReply {
  turnId: string;
  sessionId: string | null;
}

export type Phase = 'starting' | 'ready' | 'busy' | 'stopping';

export class HttpError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

interface ActiveTurn {
  turnId: string;
  started: boolean;
  stopRequested: boolean;
  done: Promise<void>;
  resolveDone: () => void;
}

const CONTINUE_PROMPT = 'Continue.';
const PRECHECK_TIMEOUT_MS = 60_000;

/** One Computer's runtime state: admission, the active turn, the session record, harness restarts. */
export class Computer {
  private base: 'starting' | 'ready' | 'stopping' = 'starting';
  private session: SessionRecord | null = null;
  private resumePending = false;
  private active: ActiveTurn | null = null;
  private restarting = false;
  private restartRequested = false;
  private readonly inflight = new Map<string, { hash: string; reply: Promise<TurnReply> }>();
  private adapterCredGeneration: number | null = null;

  constructor(
    private readonly cfg: Config,
    private readonly journal: Journal,
    private readonly adapter: Adapter,
    private readonly mcpServers: Record<string, McpServer>,
  ) {}

  get phase(): Phase {
    if (this.base !== 'ready') return this.base;
    return this.active || this.restarting ? 'busy' : 'ready';
  }

  private get sessionPath(): string {
    return join(this.cfg.stateDir, 'session.json');
  }

  /** Wake: load the session record, precheck it where the harness allows, then open admission. */
  async start(): Promise<void> {
    this.lifecycle('starting');
    try {
      this.session = JSON.parse(readFileSync(this.sessionPath, 'utf8')) as SessionRecord;
    } catch {
      this.session = null;
    }
    const wiring = wireCredentials(this.cfg);
    this.adapterCredGeneration = credentialGeneration(this.cfg);
    if (wiring.missing.length) this.journal.append('diagnostic', { message: 'credentials not injected', names: wiring.missing });
    if (this.session) {
      this.resumePending = true;
      const reason = await this.precheck(this.session);
      if (reason) this.journal.append('resume.failed', { sessionId: this.session.sessionId, reason, at: 'wake' }, { session: this.session.sessionId });
    }
    this.base = 'ready';
    this.lifecycle('ready');
  }

  status(): Record<string, unknown> {
    return {
      phase: this.phase,
      session: this.session?.sessionId ?? null,
      activeTurn: this.active?.turnId ?? null,
      harness: { kind: this.cfg.harness.kind, version: this.cfg.harness.version, model: this.cfg.harness.model },
      credentialGeneration: credentialGeneration(this.cfg),
      resumePending: this.resumePending,
    };
  }

  /** POST /v1/turns. Resolves once the turn is journaled. Idempotent by requestId. */
  submitTurn(req: TurnRequest): Promise<TurnReply> {
    const hash = createHash('sha256').update(JSON.stringify([req.session, req.prompt, req.cwd ?? null])).digest('hex');
    const prior = this.journal.findRequest(req.requestId) ?? this.inflight.get(req.requestId);
    if (prior) {
      const priorHash = 'bodyHash' in prior ? prior.bodyHash : prior.hash;
      if (priorHash !== hash) return Promise.reject(new HttpError(409, 'requestId was already used with a different body'));
      if ('reply' in prior) return prior.reply;
      return Promise.resolve({ turnId: prior.turnId, sessionId: this.journal.turnInfo(prior.turnId)?.session ?? null });
    }
    if (this.base !== 'ready') return Promise.reject(new HttpError(503, `computer is ${this.base}`));
    if (this.active || this.restarting) return Promise.reject(new HttpError(409, 'computer is busy'));
    const reply = this.beginTurn(req, hash).finally(() => this.inflight.delete(req.requestId));
    this.inflight.set(req.requestId, { hash, reply });
    return reply;
  }

  private async beginTurn(req: TurnRequest, hash: string): Promise<TurnReply> {
    const turnId = `t_${randomBytes(8).toString('hex')}`;
    let resolveDone!: () => void;
    const done = new Promise<void>((r) => (resolveDone = r));
    const active: ActiveTurn = { turnId, started: false, stopRequested: false, done, resolveDone };
    this.active = active;

    const previous = this.session;
    const wanted = req.session === 'new' ? null : req.session === 'current' ? (previous?.sessionId ?? null) : req.session;
    const cwd = req.cwd ?? previous?.cwd ?? this.cfg.agentHome;
    const started = {
      harness: { kind: this.cfg.harness.kind, version: this.cfg.harness.version },
      model: this.cfg.harness.model,
      resumed: wanted !== null,
      prompt: req.prompt,
      cwd,
    };

    let run;
    try {
      if (req.session === 'current' && previous && previous.harness !== this.cfg.harness.kind) {
        throw new ResumeError(`session ${previous.sessionId} belongs to ${previous.harness}, not ${this.cfg.harness.kind}`);
      }
      wireCredentials(this.cfg);
      const generation = credentialGeneration(this.cfg);
      if (generation !== this.adapterCredGeneration) {
        await this.adapter.restart(); // rotated credentials must not stay in a long-lived harness
        this.adapterCredGeneration = generation;
      }
      run = await this.adapter.startTurn({
        prompt: req.prompt,
        sessionId: wanted,
        cwd,
        model: this.cfg.harness.model,
        autonomy: this.cfg.autonomy,
        mcpServers: this.mcpServers,
      });
    } catch (err) {
      const message = (err as Error).message;
      const scope = { turn: turnId, session: wanted };
      this.journal.acceptTurn(req.requestId, turnId, hash, started, wanted);
      if (err instanceof ResumeError) {
        this.resumePending = true;
        this.journal.append('resume.failed', { sessionId: wanted, reason: message, at: 'prompt' }, scope);
      } else {
        this.journal.append('diagnostic', { message: `harness failed to start the turn: ${message}` }, scope);
      }
      this.journal.append('turn.finished', { status: 'failed', usage: null, error: message }, scope);
      this.endTurn(active);
      return { turnId, sessionId: wanted };
    }

    const sessionId = run.sessionId;
    this.journal.acceptTurn(req.requestId, turnId, hash, started, sessionId);
    if (sessionId !== previous?.sessionId) {
      this.journal.append('session.changed', { from: previous?.sessionId ?? null, to: sessionId }, { turn: turnId, session: sessionId });
    }
    this.saveSession({ sessionId, harness: this.cfg.harness.kind, cwd });
    if (wanted === null) this.resumePending = false;
    active.started = true;
    if (active.stopRequested) void this.adapter.stop();
    void this.pump(active, sessionId, wanted !== null, run.events);
    return { turnId, sessionId };
  }

  private async pump(active: ActiveTurn, sessionId: string, resumed: boolean, events: AsyncIterable<AdapterEvent>): Promise<void> {
    let session = sessionId;
    let status: TurnStatus | null = null;
    try {
      for await (const ev of events) {
        if (status) break;
        if (ev.type === 'session.changed') {
          session = ev.data.to;
          this.saveSession({ sessionId: session, harness: this.cfg.harness.kind, cwd: this.session?.cwd ?? this.cfg.agentHome });
        }
        if (ev.type === 'turn.finished') status = ev.data.status;
        this.journal.append(ev.type, ev.data, { turn: active.turnId, session });
      }
    } catch (err) {
      this.journal.append('diagnostic', { message: `adapter stream failed: ${(err as Error).message}` }, { turn: active.turnId, session });
    }
    if (!status) {
      status = 'failed';
      this.journal.append('turn.finished', { status, usage: null, error: 'adapter ended without turn.finished' }, { turn: active.turnId, session });
    }
    if (resumed && status === 'completed') this.resumePending = false;
    this.endTurn(active);
    if (this.restartRequested) void this.restartHarness(active.turnId, status);
  }

  private endTurn(active: ActiveTurn): void {
    if (this.active === active) this.active = null;
    active.resolveDone();
  }

  /** POST /v1/turns/{id}/stop. Idempotent. */
  stopTurn(turnId: string): { turnId: string; status: string } {
    const a = this.active;
    if (a && a.turnId === turnId) {
      if (!a.stopRequested) {
        a.stopRequested = true;
        if (a.started) void this.adapter.stop();
      }
      return { turnId, status: 'stopping' };
    }
    const info = this.journal.turnInfo(turnId);
    if (!info) throw new HttpError(404, 'unknown turn');
    return { turnId, status: info.finished ?? 'stopping' };
  }

  diagnostic(message: string, data: unknown): number {
    const scope = { turn: this.active?.turnId ?? null, session: this.session?.sessionId ?? null };
    return this.journal.append('diagnostic', { message, data, source: 'controller' }, scope).seq;
  }

  /** restart_harness: after the current turn ends, restart, resume the session, and continue. */
  requestRestart(): string {
    if (this.active) {
      this.restartRequested = true;
      return 'The harness will restart when this turn ends, resume this session, and continue with a new turn.';
    }
    void this.restartHarness(null, 'interrupted');
    return 'No turn is running; restarting the harness now.';
  }

  private async restartHarness(afterTurn: string | null, status: TurnStatus): Promise<void> {
    this.restartRequested = false;
    this.restarting = true;
    let resumed = false;
    const session = this.session;
    try {
      await this.adapter.restart();
      if (session) {
        const reason = await this.precheck(session);
        if (reason) {
          this.resumePending = true;
          this.journal.append('resume.failed', { sessionId: session.sessionId, reason, at: 'restart' }, { session: session.sessionId });
        } else resumed = true;
      }
      this.journal.append('harness.restarted', { afterTurn, sessionId: session?.sessionId ?? null, resumed }, { session: session?.sessionId ?? null });
    } catch (err) {
      this.journal.append('diagnostic', { message: `harness restart failed: ${(err as Error).message}` });
    } finally {
      this.restarting = false;
    }
    // A human stop (interrupted) or a failure does not get an automatic continuation.
    if (resumed && status === 'completed' && afterTurn) {
      await this.submitTurn({ requestId: `continue-${afterTurn}`, session: 'current', prompt: CONTINUE_PROMPT }).catch((err: Error) =>
        this.journal.append('diagnostic', { message: `continuation turn not started: ${err.message}` }),
      );
    }
  }

  /** Closes admission, interrupts the active turn and waits (bounded) for it to finish. */
  async shutdown(flushMs = 20_000): Promise<void> {
    if (this.base === 'stopping') return;
    this.base = 'stopping';
    this.lifecycle('stopping');
    const a = this.active;
    if (a) {
      this.stopTurn(a.turnId);
      await Promise.race([a.done, new Promise((r) => setTimeout(r, flushMs).unref())]);
    }
    await this.adapter.close();
  }

  private async precheck(session: SessionRecord): Promise<string | null> {
    if (session.harness !== this.cfg.harness.kind) return `session ${session.sessionId} belongs to ${session.harness}, not ${this.cfg.harness.kind}`;
    let timer: NodeJS.Timeout | undefined;
    const timeout = new Promise<string>((r) => (timer = setTimeout(() => r('precheck timed out'), PRECHECK_TIMEOUT_MS)));
    try {
      return await Promise.race([this.adapter.precheck(session.sessionId).catch((err: Error) => err.message), timeout]);
    } finally {
      clearTimeout(timer);
    }
  }

  private saveSession(record: SessionRecord): void {
    const same = this.session && JSON.stringify(this.session) === JSON.stringify(record);
    this.session = record;
    if (same) return;
    const tmp = `${this.sessionPath}.tmp`;
    writeFileSync(tmp, `${JSON.stringify(record)}\n`, { mode: 0o600 });
    if (this.cfg.isRoot) chownSync(tmp, 0, 0);
    renameSync(tmp, this.sessionPath);
  }

  private lifecycle(phase: string): void {
    this.journal.append(
      'lifecycle',
      { phase, generation: this.cfg.generation, harness: this.cfg.harness, imageTier: this.cfg.imageTier, resumePending: this.resumePending },
      { session: this.session?.sessionId ?? null },
    );
  }
}
