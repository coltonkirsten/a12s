import { createHash, randomBytes } from 'node:crypto';
import { chownSync, existsSync, readFileSync, renameSync, rmSync, watch, writeFileSync, type FSWatcher } from 'node:fs';
import { join } from 'node:path';
import { ResumeError, type Adapter, type AdapterEvent, type McpServer, type RunningTurn, type TurnStatus } from './adapters/adapter.js';
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
  /** The Controller's Batch description, copied into turn.started. */
  inbox?: Record<string, unknown>;
  /** Runs the owed restart_harness continuation; requestId must be its id and prompt is computerd's own. */
  continuation?: boolean;
  /** Recovery replay: return the turn if this requestId is known, else refuse it for good. */
  lookupOnly?: boolean;
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

/**
 * One Computer's runtime state: admission, the active turn, the session record, harness restarts. The harness
 * session outlives turns: a turn it starts by itself, when background work ends, runs in `background`.
 */
export class Computer {
  private base: 'starting' | 'ready' | 'stopping' = 'starting';
  private session: SessionRecord | null = null;
  /** Why session.json could not be used; `session: current` fails until `new`, an explicit id, or a repair. */
  private sessionDamaged: string | null = null;
  private resumePending = false;
  private active: ActiveTurn | null = null;
  private background: ActiveTurn | null = null;
  private restarting = false;
  private restartRequested = false;
  private readonly inflight = new Map<string, { hash: string; reply: Promise<TurnReply> }>();
  /** The restart_harness continuation owed to the Controller, by id. */
  private owedContinuation: { id: string; parentTurn: string | null } | null = null;
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
  ) {
    adapter.listen({
      backgroundTurn: (run) => this.beginBackgroundTurn(run),
      taskEvent: (ev) => this.journal.append(ev.type, ev.data, { session: this.session?.sessionId ?? null }),
    });
  }

  get phase(): Phase {
    if (this.base !== 'ready') return this.base;
    return this.active || this.background || this.restarting ? 'busy' : 'ready';
  }

  /** The turn the harness is running or about to run: a requested one first. */
  private get running(): ActiveTurn | null {
    return this.active ?? this.background;
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
    this.owedContinuation = this.journal.owedContinuation();
    // A continuation accepted before the crash, and never dispatched (its owed flag clears with the dispatch), was
    // just ended interrupted above. It failed before dispatch: abandon it instead of owing it forever.
    const owed = this.owedContinuation;
    const accepted = owed ? this.journal.findRequest(owed.id) : undefined;
    if (owed && accepted) {
      this.owedContinuation = null;
      this.journal.append('continuation.abandoned', { continuation: owed.id, reason: 'computerd restarted before the harness received the continuation' }, { turn: accepted.turnId });
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
      activeTurn: this.running?.turnId ?? null,
      background: { tasks: this.adapter.backgroundTasks() },
      harness: { kind: this.cfg.harness.kind, version: this.cfg.harness.version, model: this.cfg.harness.model },
      credentialGeneration: this.ackedGeneration,
      resumePending: this.resumePending,
      continuation: this.owedContinuation?.id ?? null,
    };
  }

  /** POST /v1/turns. Resolves once the turn is journaled. Idempotent by requestId. */
  submitTurn(req: TurnRequest): Promise<TurnReply> {
    if (req.continuation) req = { ...req, prompt: CONTINUE_PROMPT, session: 'current' };
    // lookupOnly is a mode of the call, not part of the request it replays.
    const lookupOnly = req.lookupOnly === true;
    const { lookupOnly: _lo, ...body } = req;
    const hash = createHash('sha256')
      .update(JSON.stringify(body.inbox || body.continuation ? [body.session, body.prompt, body.cwd ?? null, body.inbox ?? null, body.continuation ?? false] : [body.session, body.prompt, body.cwd ?? null]))
      .digest('hex');
    // In-flight first: its reply carries the session id a new session gets only after dispatch.
    const prior = this.inflight.get(req.requestId) ?? this.journal.findRequest(req.requestId);
    if (prior) {
      const priorHash = 'bodyHash' in prior ? prior.bodyHash : prior.hash;
      if (priorHash !== hash) return Promise.reject(new HttpError(409, 'requestId was already used with a different body'));
      if ('reply' in prior) return prior.reply;
      return Promise.resolve({ turnId: prior.turnId, sessionId: this.journal.turnInfo(prior.turnId)?.session ?? null });
    }
    if (this.journal.isRefused(req.requestId)) return Promise.reject(new HttpError(404, 'unknown_request: this requestId was refused by a lookup-only replay'));
    if (lookupOnly) {
      this.journal.refuse(req.requestId);
      return Promise.reject(new HttpError(404, 'unknown_request: no turn was accepted for this requestId'));
    }
    if (this.base !== 'ready') return Promise.reject(new HttpError(503, `computer is ${this.base}`));
    if (req.continuation && this.owedContinuation?.id !== req.requestId) {
      return Promise.reject(new HttpError(409, `no continuation ${req.requestId} is owed`));
    }
    if (this.active || this.background || this.restarting) return Promise.reject(new HttpError(409, 'computer is busy'));
    const reply = this.beginTurn(body, hash).finally(() => this.inflight.delete(req.requestId));
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
    const started: Record<string, unknown> = {
      harness: { kind: this.cfg.harness.kind, version: this.cfg.harness.version },
      model: this.cfg.harness.model,
      resumed: wanted !== null,
      prompt: req.prompt,
      cwd,
      requestId: req.requestId,
    };
    if (req.inbox) started.inbox = req.inbox;
    if (req.continuation) started.continuation = req.requestId;

    // Durable acceptance first: once this commits, a retry of the same requestId can never dispatch again.
    this.journal.acceptTurn(req.requestId, turnId, hash, started, wanted);
    let run;
    try {
      await this.testHooks('accept');
      if (req.session === 'current' && this.sessionDamaged) throw new ResumeError(this.sessionDamaged);
      if (req.session === 'current' && previous && previous.harness !== this.cfg.harness.kind) {
        throw new ResumeError(`session ${previous.sessionId} belongs to ${previous.harness}, not ${this.cfg.harness.kind}`);
      }
      const generation = credentialGeneration(this.cfg);
      // Rotated credentials must not stay in a long-lived harness, but restarting it would end background work:
      // with some running, the rotation waits for the idle boundary.
      if (this.wire(turnId) && generation !== this.ackedGeneration && this.adapter.backgroundTasks() === 0) {
        await this.adapter.restart();
        this.ackedGeneration = generation;
      }
      // A stop may have landed during the awaits above: then nothing reaches the harness.
      if (active.stopRequested) throw new Error('stopped before dispatch');
      run = await this.adapter.startTurn({
        prompt: req.prompt,
        sessionId: wanted,
        cwd,
        model: this.cfg.harness.model,
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
      this.abandonContinuation(req, message, scope);
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
    void this.pump(active, sessionId, wanted !== null, run.events, req);
    return { turnId, sessionId };
  }

  /** A turn the harness started by itself: journaled like any other, with reason `background` and no request. */
  private beginBackgroundTurn(run: RunningTurn): void {
    const turnId = `t_${randomBytes(8).toString('hex')}`;
    let resolveDone!: () => void;
    const done = new Promise<void>((r) => (resolveDone = r));
    const active: ActiveTurn = { turnId, started: true, stopRequested: false, done, resolveDone };
    this.background = active;
    const started = {
      harness: { kind: this.cfg.harness.kind, version: this.cfg.harness.version },
      model: this.cfg.harness.model,
      resumed: true,
      prompt: '',
      cwd: this.session?.cwd ?? this.cfg.agentHome,
      reason: 'background',
    };
    this.journal.acceptTurn(turnId, turnId, '', started, run.sessionId);
    void this.pump(active, run.sessionId, false, run.events, { requestId: turnId, session: 'current', prompt: '' });
  }

  /**
   * End-to-end test hooks: files in the root-only state dir, so only an operator with
   * `docker exec -u 0` can arm them, and each fires once. At 'accept': `exit-after-accept` kills computerd. At
   * 'dispatch': `exit-after-dispatch` kills computerd right after turn.dispatched is on disk.
   */
  private async testHooks(at: 'accept' | 'dispatch'): Promise<void> {
    const dir = join(this.cfg.stateDir, 'test-hooks');
    const take = (name: string): string | null => {
      const p = join(dir, name);
      if (!existsSync(p)) return null;
      const v = readFileSync(p, 'utf8').trim();
      rmSync(p, { force: true });
      return v;
    };
    if (at === 'accept') {
      if (take('exit-after-accept') !== null) process.exit(70);
    } else if (take('exit-after-dispatch') !== null) process.exit(71);
  }

  /**
   * Cancels the owed continuation (`a12s inbox resume --new`: a new session has nothing to continue). Returns its
   * id, or null when none is owed.
   */
  cancelContinuation(reason: string): string | null {
    const owed = this.owedContinuation;
    if (!owed || this.active) return null;
    this.owedContinuation = null;
    this.journal.append('continuation.abandoned', { continuation: owed.id, reason, cancelled: true }, {});
    return owed.id;
  }

  /** A continuation that failed before dispatch is dropped, never retried. */
  private abandonContinuation(req: TurnRequest, reason: string, scope: { turn: string | null; session: string | null }): void {
    if (!req.continuation || this.owedContinuation?.id !== req.requestId) return;
    this.owedContinuation = null;
    this.journal.append('continuation.abandoned', { continuation: req.requestId, reason }, scope);
  }

  private async pump(active: ActiveTurn, sessionId: string, resumed: boolean, events: AsyncIterable<AdapterEvent>, req: TurnRequest): Promise<void> {
    let session = sessionId;
    let status: TurnStatus | null = null;
    let dispatched = false;
    try {
      for await (const ev of events) {
        if (status) break;
        if (ev.type === 'turn.dispatched') {
          if (dispatched) continue;
          dispatched = true;
          // The owed flag clears in the same append as the continuation's receipt.
          const cont = req.continuation && this.owedContinuation?.id === req.requestId ? req.requestId : null;
          if (cont) this.owedContinuation = null;
          this.journal.append('turn.dispatched', cont ? { continuation: cont } : {}, { turn: active.turnId, session });
          await this.testHooks('dispatch');
          continue;
        }
        if (ev.type === 'session.changed') {
          session = ev.data.to;
          this.saveSession({ sessionId: session, harness: this.cfg.harness.kind, cwd: this.session?.cwd ?? this.cfg.agentHome });
        }
        if (ev.type === 'resume.failed') this.resumePending = true; // the session record stays as it was
        if (ev.type === 'turn.finished') {
          status = ev.data.status;
          const tokens = tokensOf(ev.data.usage);
          if (tokens) {
            this.journal.append(ev.type, { ...ev.data, tokens }, { turn: active.turnId, session });
            continue;
          }
        }
        this.journal.append(ev.type, ev.data, { turn: active.turnId, session });
      }
    } catch (err) {
      this.journal.append('diagnostic', { message: `adapter stream failed: ${(err as Error).message}` }, { turn: active.turnId, session });
    }
    if (!dispatched) this.abandonContinuation(req, 'the harness never received the continuation', { turn: active.turnId, session });
    if (!status) {
      status = 'failed';
      this.journal.append('turn.finished', { status, usage: null, error: 'adapter ended without turn.finished' }, { turn: active.turnId, session });
    }
    if (resumed && status === 'completed') this.resumePending = false;
    this.endTurn(active);
    if (this.active || this.background) return;
    if (this.restartRequested) void this.restartHarness(active.turnId, status);
    else void this.applyCredentials();
  }

  private endTurn(active: ActiveTurn): void {
    this.wire(active.turnId);
    if (this.active === active) this.active = null;
    if (this.background === active) this.background = null;
    active.resolveDone();
  }

  /** POST /v1/turns/{id}/stop: the turn ends interrupted and the session's background work ends too. Idempotent. */
  stopTurn(turnId: string): { turnId: string; status: string } {
    const a = [this.active, this.background].find((t) => t?.turnId === turnId);
    if (a) {
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

  /** POST /v1/stop: stops whatever runs, turns and background work. Returns what it stopped. */
  stopAll(): { turns: string[]; background: number } {
    const turns: string[] = [];
    for (const t of [this.active, this.background]) {
      if (!t) continue;
      t.stopRequested = true;
      turns.push(t.turnId);
    }
    const background = this.adapter.backgroundTasks();
    void this.adapter.stop();
    return { turns, background };
  }

  diagnostic(message: string, data: unknown): number {
    const scope = { turn: this.running?.turnId ?? null, session: this.session?.sessionId ?? null };
    return this.journal.append('diagnostic', { message, data, source: 'controller' }, scope).seq;
  }

  /** The Controller's egress proxy refused a connection: one structured event, in the Turn it happened in. */
  egressDenied(host: string, port: number, reason: string): number {
    const scope = { turn: this.running?.turnId ?? null, session: this.session?.sessionId ?? null };
    return this.journal.append('egress.denied', { host, port, reason }, scope).seq;
  }

  /** restart_harness: after the current turn ends, restart, resume the session, and continue. */
  requestRestart(): string {
    if (this.running) {
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
      // A human stop (interrupted) or a failure does not get an automatic continuation. One that is owed is
      // journaled with its id and run by the Controller at the next idle boundary; computerd starts nothing.
      const continuation = resumed && status === 'completed' && afterTurn ? `c_${ulid()}` : null;
      if (continuation) this.owedContinuation = { id: continuation, parentTurn: afterTurn };
      this.journal.append(
        'harness.restarted',
        { afterTurn, sessionId: session?.sessionId ?? null, resumed, continuation, parentTurn: continuation ? afterTurn : null },
        { session: session?.sessionId ?? null },
      );
    } catch (err) {
      this.journal.append('diagnostic', { message: `harness restart failed: ${(err as Error).message}` });
    } finally {
      this.restarting = false;
    }
  }

  /** At an idle boundary, applies a new credential generation: rewire, restart the harness, then acknowledge. */
  private async applyCredentials(): Promise<void> {
    const generation = credentialGeneration(this.cfg);
    if (generation === this.ackedGeneration || generation === this.failedGeneration) return;
    if (this.base !== 'ready' || this.running || this.restarting || this.adapter.backgroundTasks() > 0) return;
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
    const running = [this.active, this.background].filter((t): t is ActiveTurn => t !== null);
    if (running.length) {
      this.stopAll();
      const timeout = new Promise((r) => setTimeout(r, flushMs).unref());
      await Promise.race([Promise.all(running.map((t) => t.done)), timeout]);
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
            this.wire(this.running?.turnId ?? null);
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

  // wireCredentials is synchronous end to end (sync fs, spawnSync), so the watcher, after-turn, prompt-time and
  // .generation paths cannot interleave inside computerd; the commit itself guards against the external injector.
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

const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';

/** A ULID: 48-bit milliseconds and 80 random bits, Crockford base32. */
export function ulid(now = Date.now()): string {
  let t = '';
  let ms = now;
  for (let i = 0; i < 10; i++) {
    t = CROCKFORD[ms % 32] + t;
    ms = Math.floor(ms / 32);
  }
  const rnd = randomBytes(16);
  let r = '';
  for (let i = 0; i < 16; i++) r += CROCKFORD[(rnd[i] ?? 0) % 32];
  return t + r;
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

/** The harness's own usage as {input, output, cacheRead, cacheWrite}; input excludes cached input in both. */
export function tokensOf(usage: Record<string, unknown> | null): Record<string, number> | null {
  if (!usage) return null;
  const n = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
  if ('inputTokens' in usage || 'outputTokens' in usage) {
    return { input: n(usage.inputTokens), output: n(usage.outputTokens), cacheRead: n(usage.cacheReadInputTokens), cacheWrite: n(usage.cacheCreationInputTokens) };
  }
  const last = usage.last;
  if (last && typeof last === 'object') {
    const l = last as Record<string, unknown>;
    return { input: Math.max(0, n(l.inputTokens) - n(l.cachedInputTokens)), output: n(l.outputTokens), cacheRead: n(l.cachedInputTokens), cacheWrite: 0 };
  }
  return null;
}
