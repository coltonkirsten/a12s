import { createHash, randomBytes } from 'node:crypto';
import { chownSync, readFileSync, renameSync, watch, writeFileSync, type FSWatcher } from 'node:fs';
import { join } from 'node:path';
import { ResumeError, type Adapter, type AdapterEvent, type McpServer, type TurnStatus } from './adapters/adapter.js';
import type { Config, HarnessKind } from './config.js';
import { credentialFiles, credentialGeneration, wireCredentials, type Wiring } from './credentials.js';
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

const CONTINUE_PROMPT = 'a12s restarted your harness at your request and resumed this session. Continue the task you were working on.';
const PRECHECK_TIMEOUT_MS = 60_000;
const RECLAIM_DEBOUNCE_MS = 200;

/** One Computer's runtime state: admission, the active turn, the session record, harness restarts. */
export class Computer {
  private base: 'starting' | 'ready' | 'stopping' = 'starting';
  private session: SessionRecord | null = null;
  /** Why session.json could not be used; `session: current` fails until `new`, an explicit id, or a repair. */
  private sessionDamaged: string | null = null;
  private resumePending = false;
  private active: ActiveTurn | null = null;
  private restarting = false;
  private restartRequested = false;
  private readonly inflight = new Map<string, { hash: string; reply: Promise<TurnReply> }>();
  /** The credential generation actually in effect: wired, and any long-lived harness restarted with it. */
  private ackedGeneration: number | null = null;
  private credentialTimer: NodeJS.Timeout | null = null;
  private failedGeneration: number | null = null;
  private watchers: FSWatcher[] = [];
  private reclaimTimer: NodeJS.Timeout | null = null;

  constructor(
    private readonly cfg: Config,
    private readonly journal: Journal,
    private readonly adapter: Adapter,
    private readonly mcpServers: Record<string, McpServer>,
    private readonly credentialPollMs = 2_000,
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
    // Never redispatch: a turn accepted before a crash ends interrupted, and "continue" is the human's call.
    for (const t of this.journal.unfinishedTurns()) {
      this.journal.append('turn.finished', { status: 'interrupted', usage: null, error: 'computerd restarted' }, { turn: t.turnId, session: t.session });
    }
    const loaded = loadSessionRecord(this.sessionPath);
    this.session = loaded.record;
    this.sessionDamaged = loaded.damaged;
    if (this.sessionDamaged) {
      this.resumePending = true;
      this.journal.append('resume.failed', { sessionId: null, reason: this.sessionDamaged, at: 'wake' });
    }
    const generation = credentialGeneration(this.cfg); // read before wiring: never acknowledge more than was applied
    const wired = this.wire(null);
    if (this.session) {
      this.resumePending = true;
      const reason = await this.precheck(this.session);
      if (reason) this.journal.append('resume.failed', { sessionId: this.session.sessionId, reason, at: 'wake' }, { session: this.session.sessionId });
    }
    if (wired) this.ackedGeneration = generation; // no harness process holds older credentials yet
    this.base = 'ready';
    this.lifecycle('ready');
    this.credentialTimer = setInterval(() => void this.applyCredentials(), this.credentialPollMs);
    this.credentialTimer.unref();
    this.watchCredentialFiles();
  }

  status(): Record<string, unknown> {
    return {
      phase: this.phase,
      session: this.session?.sessionId ?? null,
      activeTurn: this.active?.turnId ?? null,
      harness: { kind: this.cfg.harness.kind, version: this.cfg.harness.version, model: this.cfg.harness.model },
      credentialGeneration: this.ackedGeneration,
      resumePending: this.resumePending,
    };
  }

  /** POST /v1/turns. Resolves once the turn is journaled. Idempotent by requestId. */
  submitTurn(req: TurnRequest): Promise<TurnReply> {
    const hash = createHash('sha256').update(JSON.stringify([req.session, req.prompt, req.cwd ?? null])).digest('hex');
    // In-flight first: its reply carries the session id a new session gets only after dispatch.
    const prior = this.inflight.get(req.requestId) ?? this.journal.findRequest(req.requestId);
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

    // Durable acceptance first: once this commits, a retry of the same requestId can never dispatch again.
    this.journal.acceptTurn(req.requestId, turnId, hash, started, wanted);
    let run;
    try {
      if (req.session === 'current' && this.sessionDamaged) throw new ResumeError(this.sessionDamaged);
      if (req.session === 'current' && previous && previous.harness !== this.cfg.harness.kind) {
        throw new ResumeError(`session ${previous.sessionId} belongs to ${previous.harness}, not ${this.cfg.harness.kind}`);
      }
      const generation = credentialGeneration(this.cfg);
      if (this.wire(turnId) && generation !== this.ackedGeneration) {
        await this.adapter.restart(); // rotated credentials must not stay in a long-lived harness
        this.ackedGeneration = generation;
      }
      // A stop may have landed during the awaits above: then nothing reaches the harness.
      if (active.stopRequested) throw new Error('stopped before dispatch');
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
      if (active.stopRequested) {
        this.journal.append('turn.finished', { status: 'interrupted', usage: null, error: 'stopped before the harness started' }, scope);
        this.endTurn(active);
        return { turnId, sessionId: wanted };
      }
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
    if (sessionId !== previous?.sessionId) {
      this.journal.append('session.changed', { from: previous?.sessionId ?? null, to: sessionId }, { turn: turnId, session: sessionId });
    }
    this.saveSession({ sessionId, harness: this.cfg.harness.kind, cwd });
    this.sessionDamaged = null;
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
        if (ev.type === 'resume.failed') this.resumePending = true; // the session record stays as it was
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
    else void this.applyCredentials();
  }

  private endTurn(active: ActiveTurn): void {
    this.wire(active.turnId);
    if (this.active === active) this.active = null;
    active.resolveDone();
  }

  /** POST /v1/turns/{id}/stop. Idempotent. */
  stopTurn(turnId: string): { turnId: string; status: string } {
    const a = this.active;
    if (a && a.turnId === turnId) {
      if (!a.stopRequested) {
        a.stopRequested = true;
        void this.adapter.stop(); // also cancels a turn the adapter is still starting
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
      const generation = credentialGeneration(this.cfg);
      const wired = this.wire(null);
      await this.adapter.restart();
      if (wired) this.ackedGeneration = generation;
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

  /** At an idle boundary, applies a new credential generation: rewire, restart the harness, then acknowledge. */
  private async applyCredentials(): Promise<void> {
    const generation = credentialGeneration(this.cfg);
    if (generation === this.ackedGeneration || generation === this.failedGeneration) return;
    if (this.base !== 'ready' || this.active || this.restarting) return;
    this.restarting = true;
    try {
      if (!this.wire(null)) {
        this.failedGeneration = generation; // retried at the next turn or the next injection, not every poll
        return;
      }
      this.failedGeneration = null;
      await this.adapter.restart();
      this.ackedGeneration = generation;
      this.journal.append('lifecycle', { phase: 'credentials-applied', credentialGeneration: generation }, { session: this.session?.sessionId ?? null });
    } catch (err) {
      this.journal.append('diagnostic', { message: `applying credential generation ${generation} failed: ${(err as Error).message}` });
    } finally {
      this.restarting = false;
    }
  }

  /** Closes admission, interrupts the active turn and waits (bounded) for it to finish. */
  async shutdown(flushMs = 20_000): Promise<void> {
    if (this.base === 'stopping') return;
    this.base = 'stopping';
    if (this.credentialTimer) clearInterval(this.credentialTimer);
    for (const w of this.watchers) w.close();
    if (this.reclaimTimer) clearTimeout(this.reclaimTimer);
    this.lifecycle('stopping');
    const a = this.active;
    if (a) {
      this.stopTurn(a.turnId);
      await Promise.race([a.done, new Promise((r) => setTimeout(r, flushMs).unref())]);
    }
    await this.adapter.close();
    this.wire(null);
  }

  /** (Re)wires credentials, moving any file a harness rewrote on the home drive back onto tmpfs. */
  /**
   * A harness refreshing its token replaces the symlink with a regular file on the home drive; reclaim it right away
   * (debounced) instead of waiting for the turn to end. The watch only yields file names; the work runs as the agent.
   */
  private watchCredentialFiles(): void {
    for (const [dir, name] of credentialFiles(this.cfg)) {
      try {
        const w = watch(dir, (_event, file) => {
          if (file !== name || this.reclaimTimer) return;
          this.reclaimTimer = setTimeout(() => {
            this.reclaimTimer = null;
            this.wire(this.active?.turnId ?? null);
          }, RECLAIM_DEBOUNCE_MS);
        });
        w.on('error', () => w.close());
        w.unref();
        this.watchers.push(w);
      } catch {
        // dir missing: the after-turn reclaim still covers it
      }
    }
  }

  /** Returns true only if every declared credential is wired; failures are journaled (names, never values). */
  private wire(turn: string | null): boolean {
    const scope = { turn, session: this.session?.sessionId ?? null };
    let w: Wiring;
    try {
      w = wireCredentials(this.cfg);
    } catch (err) {
      this.journal.append('diagnostic', { message: `credential wiring failed: ${(err as Error).message}` }, scope);
      return false;
    }
    if (w.reclaimed.length) this.journal.append('diagnostic', { message: 'refreshed credential moved back onto tmpfs', names: w.reclaimed }, scope);
    for (const n of w.notes) this.journal.append('diagnostic', { message: `credential wiring: ${n}` }, scope);
    if (w.failures.length) this.journal.append('diagnostic', { message: 'credential wiring failed', failures: w.failures }, scope);
    return w.failures.length === 0;
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

/** Distinguishes "never had a session" (no file) from a record that exists but cannot be trusted. */
export function loadSessionRecord(path: string): { record: SessionRecord | null; damaged: string | null } {
  let text: string;
  try {
    text = readFileSync(path, 'utf8');
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return { record: null, damaged: null };
    return { record: null, damaged: `session record unreadable: ${(err as Error).message}` };
  }
  try {
    const r = JSON.parse(text) as Partial<SessionRecord>;
    const ok =
      typeof r === 'object' && r !== null && typeof r.sessionId === 'string' && r.sessionId !== '' &&
      (r.harness === 'claude-code' || r.harness === 'codex') && typeof r.cwd === 'string' && r.cwd.startsWith('/');
    if (ok) return { record: { sessionId: r.sessionId!, harness: r.harness!, cwd: r.cwd! }, damaged: null };
  } catch {
    // fall through
  }
  return { record: null, damaged: 'session record is damaged (expected {sessionId, harness, cwd})' };
}
