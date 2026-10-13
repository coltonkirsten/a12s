// Runs in Node, Deno, Bun and browsers: no node: imports here (`npm run dashboard` bundles it for the browser).
import { readSSE } from "./sse.js";
import type {
  A12sEvent,
  ApiErrorBody,
  ComputerStatus,
  EnqueueRequest,
  EnqueueResponse,
  GapData,
  GapEvent,
  InboxListOptions,
  InboxListResponse,
  Message,
  PutFleetRequest,
  PutFleetResponse,
  ServiceStatus,
  StatusResponse,
  Token,
  TokenRequest,
  ToolCall,
  Turn,
  TurnInboxMessage,
} from "./types.js";

export interface A12sClientOptions {
  /**
   * The Controller's API, e.g. http://127.0.0.1:52011. Default "": the page's own origin, which is what a browser
   * app served by `a12s gateway` uses.
   */
  url?: string;
  /** The owner token, or a scoped token from `a12s token create`. Not needed behind `a12s gateway`. */
  token?: string;
  fetch?: typeof fetch;
}

/** A refused call: the HTTP status and the Controller's error, including its fix line. */
export class A12sError extends Error {
  readonly status: number;
  /** A stable code such as `busy`, `asleep`, `no_grant`, `inbox_full`, `not_found` (see docs/api.md#errors). */
  readonly code: string;
  readonly fix?: string;
  readonly details?: Record<string, string>;
  constructor(status: number, body: ApiErrorBody) {
    super(body.fix ? `${body.code}: ${body.message}  fix: ${body.fix}` : `${body.code}: ${body.message}`);
    this.name = "A12sError";
    this.status = status;
    this.code = body.code;
    this.fix = body.fix;
    this.details = body.details;
  }
}

/** A 404: the agent computer, Service, Message or token does not exist. */
export class NotFound extends A12sError {
  constructor(status: number, body: ApiErrorBody) {
    super(status, body);
    this.name = "NotFound";
  }
}

function apiError(status: number, body: ApiErrorBody): A12sError {
  return status === 404 ? new NotFound(status, body) : new A12sError(status, body);
}

/** Thrown by an event stream with `onGap: "throw"` when events were lost. */
export class A12sGapError extends Error {
  readonly after?: number;
  readonly data: GapData;
  constructor(data: GapData) {
    super(`journal gap after seq ${data.after ?? "(none)"}: events were lost; resync with status()`);
    this.name = "A12sGapError";
    this.after = data.after;
    this.data = data;
  }
}

/** An event stream received a frame that is not a valid journal event. */
export class A12sStreamError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "A12sStreamError";
  }
}

export interface EventsOptions {
  /** One Computer only: replay this Turn from its start, then follow it. */
  turn?: string;
  /** One Computer only: resume after this journal seq. Otherwise a stream starts live. */
  after?: number;
  signal?: AbortSignal;
  /** Delay before reconnecting a dropped stream. Default 1000 ms. */
  reconnectMs?: number;
  /**
   * When a stream can't resume from its cursor (the journal was reset): "yield" (default) yields a `gap` event and
   * carries on live; "throw" throws {@link A12sGapError}. Either way events were lost: resync from `status()`.
   */
  onGap?: "yield" | "throw";
}

export type SharedEventsOptions = Omit<EventsOptions, "turn" | "after">;

export interface WaitOptions {
  signal?: AbortSignal;
  /** Every event of the Turn, as it streams. */
  onEvent?: (ev: A12sEvent) => void;
}

/** What `send(...).wait()` resolves with: the finished Turn that delivered the Message, and the Message. */
export interface Reply extends Turn {
  message: EnqueueResponse;
}

const enc = encodeURIComponent;

/**
 * @deprecated Use {@link Fleet}: `fleet.agentComputers.get(name)` returns an {@link AgentComputer} with these
 * methods. Kept for one release.
 */
export class A12sClient {
  readonly url: string;
  private readonly token?: string;
  private readonly fetchImpl: typeof fetch;
  private shared?: SharedEvents;

  constructor(opts: A12sClientOptions = {}) {
    this.url = (opts.url ?? "").replace(/\/+$/, "");
    this.token = opts.token;
    this.fetchImpl = opts.fetch ?? ((...args) => fetch(...args));
  }

  private headers(extra: Record<string, string> = {}): Record<string, string> {
    return this.token ? { authorization: `Bearer ${this.token}`, ...extra } : extra;
  }

  /** @internal */
  async request<T>(
    method: string,
    path: string,
    body?: unknown,
    opts: { headers?: Record<string, string>; signal?: AbortSignal } = {},
  ): Promise<T> {
    const res = await this.fetchImpl(this.url + path, {
      method,
      headers: this.headers({ ...(body === undefined ? {} : { "content-type": "application/json" }), ...opts.headers }),
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: opts.signal,
    });
    const text = await res.text();
    if (!res.ok) throw apiError(res.status, parseError(res.status, text));
    return (text ? JSON.parse(text) : undefined) as T;
  }

  /** Every Computer and Service: phase, session, active Turn, inbox, power. */
  status(): Promise<StatusResponse> {
    return this.request("GET", "/v1/status");
  }

  async computer(name: string): Promise<ComputerStatus> {
    const c = (await this.status()).agentComputers.find((x) => x.name === name);
    if (!c) throw new NotFound(404, { code: "not_found", message: `no agent computer ${name}` });
    return c;
  }

  /**
   * Puts a Message in a Computer's inbox and returns at once. It is delivered at the Computer's next idle moment,
   * batched with anything else pending. Works while the Computer is busy or asleep.
   */
  send(computer: string, msg: EnqueueRequest | string): Promise<EnqueueResponse> {
    const m: EnqueueRequest = typeof msg === "string" ? { body: msg } : { ...msg };
    m.idempotencyKey ??= globalThis.crypto.randomUUID();
    return this.request("POST", `/v1/agent-computers/${enc(computer)}/inbox`, m);
  }

  /** Sends a Message, waits for the Turn that delivers it and resolves when that Turn ends. */
  async prompt(computer: string, body: string, opts: Omit<EnqueueRequest, "body"> & WaitOptions = {}): Promise<Reply> {
    const { signal, onEvent, ...msg } = opts;
    return this.reply(computer, await this.send(computer, { ...msg, body }), { signal, onEvent });
  }

  /**
   * @internal Waits for the Turn that delivers the Message and resolves when it ends. Delivery is read from the
   * Message's own snapshot and the Turn is replayed from its start, so it works however late it is called.
   */
  async reply(computer: string, message: EnqueueResponse, opts: WaitOptions = {}): Promise<Reply> {
    const { signal, onEvent } = opts;
    const turn = await this.deliveredIn(computer, message.id, signal);
    const builder = new TurnBuilder();
    // A gap mid-Turn would silently drop text: fail loudly instead.
    for await (const ev of this.events(computer, { turn, signal, onGap: "throw" })) {
      onEvent?.(ev);
      const done = builder.add(ev);
      if (done && done.id === turn) return { ...done, message };
    }
    throw new Error(`event stream for turn ${turn} ended early`);
  }

  /**
   * Live events of one Computer, several, or (no argument) every Computer in the Fleet, merged. Each event carries
   * its `computer`. Reconnects after a network drop without losing or repeating events. Call `await stream.ready()`
   * before sending work you want to see from the start; `close()` (or `break`) ends it.
   */
  events(computers?: string | string[], opts: EventsOptions = {}): EventStream {
    const names = typeof computers === "string" ? [computers] : computers;
    if ((opts.turn !== undefined || opts.after !== undefined) && names?.length !== 1) {
      throw new TypeError("events: turn and after need exactly one agent computer");
    }
    return new EventStream((name, live, signal) => {
      const q = opts.turn ? `?turn=${enc(opts.turn)}` : "";
      const events = this.stream(name, `/v1/agent-computers/${enc(name)}/events${q}`, opts.after, { ...opts, signal, live });
      return opts.turn ? untilTurnFinished(events) : events;
    }, names ?? (async () => (await this.status()).agentComputers.map((c) => c.name)), opts.signal);
  }

  /**
   * @internal Events of the named agent computers (all if none), and the Controller's own about them, read from this
   * client's one GET /v1/events connection.
   */
  sharedEvents(names: string[] | undefined, opts: SharedEventsOptions = {}): EventStream {
    this.shared ??= new SharedEvents((cursor, signal) =>
      this.fetchImpl(this.url + "/v1/events", {
        headers: this.headers({ accept: "text/event-stream", ...(cursor !== undefined ? { "last-event-id": cursor } : {}) }),
        signal,
      }),
    );
    const shared = this.shared;
    const wanted = names && new Set(names);
    return new EventStream(
      (_name, live, signal) => shared.subscribe({ wanted, live, onGap: opts.onGap, reconnectMs: opts.reconnectMs }, signal),
      [""],
      opts.signal,
    );
  }

  /**
   * Finished Turns of one Computer, several or all, as they end: who sent the work (`messages`), what the agent said
   * (`text`) and how it ended (`status`). Same `ready()` and `close()` as {@link events}.
   */
  turns(computers?: string | string[], opts: Omit<EventsOptions, "turn" | "after"> = {}): TurnStream {
    return new TurnStream(this.events(computers, opts));
  }

  inbox(computer: string, opts: InboxListOptions = {}): Promise<InboxListResponse> {
    const q = new URLSearchParams();
    if (opts.state) q.set("state", opts.state);
    if (opts.bodies) q.set("bodies", "true");
    if (opts.after !== undefined) q.set("after", String(opts.after));
    const qs = q.toString();
    return this.request("GET", `/v1/agent-computers/${enc(computer)}/inbox${qs ? "?" + qs : ""}`);
  }

  message(computer: string, id: string): Promise<Message> {
    return this.request("GET", `/v1/agent-computers/${enc(computer)}/inbox/${enc(id)}`);
  }

  /** Withdraws a pending Message; a 409 A12sError names its state if it is too late. */
  withdraw(computer: string, id: string): Promise<Message> {
    return this.request("DELETE", `/v1/agent-computers/${enc(computer)}/inbox/${enc(id)}`);
  }

  /** Interrupts a Turn (default: the active one). The Turn ends `interrupted`; nothing is replayed. */
  async stop(computer: string, turn?: string): Promise<void> {
    const id = turn ?? (await this.computer(computer)).activeTurn;
    if (id) await this.request("POST", `/v1/agent-computers/${enc(computer)}/turns/${enc(id)}/stop`, {});
  }

  /**
   * Stops the agent computer's processes and keeps its disks, session and inbox. A Message from a sender its power
   * policy lists (wakeOnMessage) wakes it; with `hold` it stays asleep until wake().
   */
  async sleep(computer: string, opts: { hold?: boolean } = {}): Promise<void> {
    await this.request("POST", `/v1/agent-computers/${enc(computer)}/power`, {
      state: "asleep",
      ...(opts.hold ? { hold: true } : {}),
    });
  }

  /** Starts a sleeping agent computer; its session resumes on the next Turn. `clearInbox` discards pending Messages. */
  async wake(computer: string, opts: { clearInbox?: boolean } = {}): Promise<void> {
    await this.request("POST", `/v1/agent-computers/${enc(computer)}/power`, {
      state: "awake",
      ...(opts.clearInbox ? { clearInbox: true, requestId: crypto.randomUUID() } : {}),
    });
  }

  /** Holds the inbox: Messages still queue, none is delivered until resume(). */
  hold(computer: string, opts: { reason?: string } = {}): Promise<InboxListResponse> {
    return this.request("POST", `/v1/agent-computers/${enc(computer)}/inbox/hold`, opts.reason ? { reason: opts.reason } : {});
  }

  /** Releases a hold; pending Messages are delivered at the next idle moment. */
  resume(computer: string): Promise<InboxListResponse> {
    return this.request("POST", `/v1/agent-computers/${enc(computer)}/inbox/resume`, {});
  }

  async services(): Promise<ServiceStatus[]> {
    return (await this.request<{ services: ServiceStatus[] }>("GET", "/v1/services")).services ?? [];
  }

  service(name: string): Promise<ServiceStatus> {
    return this.request("GET", `/v1/services/${enc(name)}`);
  }

  /** A scoped token for another program; owner only. The secret is in the response once. */
  createToken(req: TokenRequest): Promise<Token> {
    return this.request("POST", "/v1/tokens", req);
  }

  /** What `a12s up` sends: validate and apply a whole Fleet. Most apps leave this to the CLI. */
  putFleet(req: PutFleetRequest): Promise<PutFleetResponse> {
    return this.request("PUT", "/v1/fleet", req);
  }

  /** Waits until the Message is delivered and returns the Turn that carries it. */
  private async deliveredIn(computer: string, id: string, signal?: AbortSignal): Promise<string> {
    // Every subscription starts with a snapshot, so resubscribing after a drop never misses the delivery.
    while (!signal?.aborted) {
      for await (const ev of this.stream(computer, `/v1/agent-computers/${enc(computer)}/inbox/${enc(id)}/events`, undefined, {
        signal,
        once: true,
      })) {
        const d = (ev.data ?? {}) as any;
        const type = ev.type as string;
        const state: string = type === "snapshot" ? d.message?.state : type;
        const turn: string | undefined = type === "snapshot" ? d.message?.turn : d.turn;
        if (state === "delivered" && turn) return turn;
        if (["withdrawn", "discarded", "expired", "replaced"].includes(state)) {
          throw new A12sError(409, { code: state, message: `message ${id} was ${state} before delivery` });
        }
        if ((type === "snapshot" && d.held) || type === "held") {
          throw new A12sError(409, {
            code: "held",
            message: `${computer}'s inbox is held (${d.holdReason ?? d.reason}); ${id} waits in the inbox`,
            fix: `a12s inbox resume ${computer}`,
          });
        }
      }
      await sleep(1000, signal);
    }
    throw signal.reason ?? new Error("aborted");
  }

  private async *stream(
    computer: string,
    path: string,
    after: number | undefined,
    opts: { signal?: AbortSignal; reconnectMs?: number; once?: boolean; onGap?: "yield" | "throw"; live?: () => void },
  ): AsyncGenerator<A12sEvent> {
    let cursor = after;
    for (;;) {
      if (opts.signal?.aborted) return;
      let res: Response;
      try {
        res = await this.fetchImpl(this.url + path, {
          headers: this.headers({
            accept: "text/event-stream",
            ...(cursor !== undefined ? { "last-event-id": String(cursor) } : {}),
          }),
          signal: opts.signal,
        });
      } catch (err) {
        if (opts.signal?.aborted) return;
        if (!isNetworkError(err)) throw err;
        await sleep(opts.reconnectMs ?? 1000, opts.signal);
        continue;
      }
      if (!res.ok) {
        const err = apiError(res.status, parseError(res.status, await res.text()));
        // Asleep or starting: the journal comes back with the Computer.
        if (res.status !== 503 && res.status !== 502) throw err;
        opts.live?.();
        await sleep(opts.reconnectMs ?? 1000, opts.signal);
        continue;
      }
      if (!res.body) throw new A12sStreamError(`event stream ${path}: response has no body`);
      // Only reading the body is retried: a read that fails is the connection dropping. A malformed frame or an
      // exception thrown into the generator ends the stream instead of looping forever.
      const frames = readSSE(res.body);
      try {
        for (;;) {
          let r: IteratorResult<{ id?: string; event?: string; data: string }>;
          try {
            r = await frames.next();
          } catch {
            break; // dropped: reconnect from the cursor
          }
          if (r.done) break;
          const f = r.value;
          if (f.event === "head") {
            // Everything up to the head has been sent: a reconnect resumes from there even if no event came.
            const seq = (parseJSON(f.data) as { seq?: number } | undefined)?.seq;
            if (typeof seq === "number") cursor = Math.max(cursor ?? 0, seq);
            opts.live?.();
            continue;
          }
          if (f.event === "gap") {
            const server = parseJSON(f.data);
            const data: GapData = { ...(server && typeof server === "object" ? server : {}), after: cursor };
            cursor = undefined; // the stream goes live; a reconnect starts live too
            if (opts.onGap === "throw") throw new A12sGapError(data);
            const gap: GapEvent = { v: 1, computer, session: "", turn: "", seq: 0, ts: new Date().toISOString(), type: "gap", data };
            yield gap;
            continue;
          }
          if (f.event || !f.data) continue;
          let ev: A12sEvent;
          try {
            ev = JSON.parse(f.data) as A12sEvent;
          } catch (err) {
            throw new A12sStreamError(`malformed event after seq ${cursor ?? "(start)"}: ${f.data.slice(0, 120)}`, {
              cause: err,
            });
          }
          if (f.id !== undefined) cursor = Number(f.id);
          yield ev;
        }
      } finally {
        await frames.return(undefined);
        res.body.cancel().catch(() => {});
      }
      if (opts.signal?.aborted) return;
      if (opts.once) return;
      await sleep(opts.reconnectMs ?? 1000, opts.signal);
    }
  }
}

interface Subscriber {
  wanted?: Set<string>;
  onGap?: "yield" | "throw";
  reconnectMs?: number;
  live: () => void;
  push: (ev: A12sEvent) => void;
  fail: (err: unknown) => void;
}

/** One GET /v1/events connection, open while anything subscribes: watching fifty agent computers takes one. */
class SharedEvents {
  private readonly subs = new Set<Subscriber>();
  private conn?: AbortController;
  private live = false;

  constructor(private readonly open: (cursor: string | undefined, signal: AbortSignal) => Promise<Response>) {}

  async *subscribe(sub: Omit<Subscriber, "push" | "fail">, signal: AbortSignal): AsyncGenerator<A12sEvent> {
    const queue: A12sEvent[] = [];
    let failure: { error: unknown } | undefined;
    let wake: (() => void) | undefined;
    const notify = () => {
      const w = wake;
      wake = undefined;
      w?.();
    };
    const s: Subscriber = {
      ...sub,
      push: (ev) => {
        queue.push(ev);
        notify();
      },
      fail: (error) => {
        failure ??= { error };
        notify();
      },
    };
    signal.addEventListener("abort", notify, { once: true });
    this.add(s);
    try {
      for (;;) {
        if (queue.length) {
          yield queue.shift()!;
          continue;
        }
        if (failure) throw failure.error;
        if (signal.aborted) return;
        await new Promise<void>((r) => (wake = r));
      }
    } finally {
      signal.removeEventListener("abort", notify);
      this.remove(s);
    }
  }

  private add(s: Subscriber): void {
    this.subs.add(s);
    if (!this.conn) {
      const conn = new AbortController();
      this.conn = conn;
      this.live = false;
      void this.run(conn);
    } else if (this.live) {
      s.live();
    }
  }

  private remove(s: Subscriber): void {
    this.subs.delete(s);
    if (this.subs.size === 0) this.stop();
  }

  private stop(): void {
    this.conn?.abort();
    this.conn = undefined;
    this.live = false;
  }

  private async run(conn: AbortController): Promise<void> {
    const { signal } = conn;
    let cursor: string | undefined;
    try {
      while (!signal.aborted) {
        let res: Response | undefined;
        try {
          res = await this.open(cursor, signal);
        } catch (err) {
          if (signal.aborted) return;
          if (!isNetworkError(err)) throw err;
        }
        if (res && !res.ok) {
          const err = apiError(res.status, parseError(res.status, await res.text()));
          if (res.status !== 502 && res.status !== 503) throw err;
          res = undefined;
        }
        if (res) {
          if (!res.body) throw new A12sStreamError("GET /v1/events: response has no body");
          const frames = readSSE(res.body);
          try {
            for (;;) {
              let r: IteratorResult<{ id?: string; event?: string; data: string }>;
              try {
                r = await frames.next();
              } catch {
                break;
              }
              if (r.done) break;
              cursor = this.frame(r.value, cursor);
            }
          } finally {
            await frames.return(undefined);
            res.body.cancel().catch(() => {});
          }
        }
        await sleep(this.reconnectMs(), signal);
      }
    } catch (err) {
      if (signal.aborted) return;
      const subs = [...this.subs];
      if (this.conn === conn) this.stop();
      for (const s of subs) s.fail(err);
    }
  }

  /** Delivers one frame and returns the cursor to resume after. */
  private frame(f: { id?: string; event?: string; data: string }, cursor: string | undefined): string | undefined {
    if (f.event === "head") {
      const id = (parseJSON(f.data) as { id?: unknown } | undefined)?.id;
      this.live = true;
      for (const s of this.subs) s.live();
      return typeof id === "string" && id ? id : cursor;
    }
    if (f.event === "gap") {
      const data: GapData = cursor !== undefined ? { lastEventId: cursor } : {};
      const gap: GapEvent = { v: 1, computer: "", session: "", turn: "", seq: 0, ts: new Date().toISOString(), type: "gap", data };
      for (const s of this.subs) {
        if (s.onGap === "throw") s.fail(new A12sGapError(data));
        else s.push(gap);
      }
      return cursor;
    }
    if (f.event || !f.data) return cursor;
    let ev: A12sEvent;
    try {
      ev = JSON.parse(f.data) as A12sEvent;
    } catch (err) {
      throw new A12sStreamError(`malformed event after ${cursor ?? "(start)"}: ${f.data.slice(0, 120)}`, { cause: err });
    }
    for (const s of this.subs) if (wanted(s.wanted, ev)) s.push(ev);
    return f.id ?? cursor;
  }

  private reconnectMs(): number {
    let ms = 1000;
    for (const s of this.subs) if (s.reconnectMs !== undefined) ms = Math.min(ms, s.reconnectMs);
    return ms;
  }
}

function agentComputerOf(ev: A12sEvent): string {
  return ev.agentComputer || ev.computer;
}

function wanted(names: Set<string> | undefined, ev: A12sEvent): boolean {
  if (!names) return true;
  if (ev.type === "fleet.changed") return (ev.data?.names ?? []).some((n) => names.has(n));
  if (ev.type === "service.phase") return names.has(ev.data?.service);
  return names.has(agentComputerOf(ev));
}

type Source = (computer: string, live: () => void, signal: AbortSignal) => AsyncGenerator<A12sEvent>;

/** A Turn replay: its events up to and including its turn.finished. The server keeps the stream open after. */
async function* untilTurnFinished(events: AsyncGenerator<A12sEvent>): AsyncGenerator<A12sEvent> {
  try {
    for await (const ev of events) {
      yield ev;
      if (ev.type === "turn.finished") return;
    }
  } finally {
    await events.return(undefined);
  }
}

/** Events of one or more Computers, merged in arrival order. Iterate it with `for await`. */
export class EventStream implements AsyncIterableIterator<A12sEvent> {
  private readonly ac = new AbortController();
  private readonly queue: A12sEvent[] = [];
  private started?: Promise<void>;
  private remaining?: number;
  private failure: { error: unknown } | undefined;
  private wake?: () => void;
  private readonly readyP: Promise<void>;
  private resolveReady!: () => void;
  private rejectReady!: (err: unknown) => void;
  private isReady = false;

  constructor(
    private readonly source: Source,
    private readonly names: string[] | (() => Promise<string[]>),
    signal?: AbortSignal,
  ) {
    this.readyP = new Promise<void>((resolve, reject) => {
      this.resolveReady = () => {
        this.isReady = true;
        resolve();
      };
      this.rejectReady = reject;
    });
    this.readyP.catch(() => {}); // surfaced through next() when nobody awaits ready()
    if (signal?.aborted) this.ac.abort();
    signal?.addEventListener("abort", () => this.close(), { once: true });
  }

  /**
   * Resolves once every stream is connected and live (a sleeping Computer counts: its events start when it wakes).
   * Events that happen after this are never missed.
   */
  ready(): Promise<void> {
    this.start();
    return this.readyP;
  }

  close(): void {
    this.ac.abort();
    this.resolveReady();
    this.notify();
  }

  async next(): Promise<IteratorResult<A12sEvent>> {
    this.start();
    for (;;) {
      if (this.queue.length) return { value: this.queue.shift()!, done: false };
      if (this.failure) {
        const { error } = this.failure;
        this.failure = undefined;
        this.close();
        throw error;
      }
      if (this.ac.signal.aborted || this.remaining === 0) return { value: undefined, done: true };
      await new Promise<void>((r) => (this.wake = r));
    }
  }

  async return(): Promise<IteratorResult<A12sEvent>> {
    this.close();
    return { value: undefined, done: true };
  }

  async throw(err?: unknown): Promise<IteratorResult<A12sEvent>> {
    this.close();
    throw err;
  }

  [Symbol.asyncIterator](): this {
    return this;
  }

  private start(): void {
    this.started ??= (async () => {
      const names = typeof this.names === "function" ? await this.names() : this.names;
      this.remaining = names.length;
      let live = 0;
      if (names.length === 0) this.resolveReady();
      for (const name of names) {
        let counted = false;
        const onLive = () => {
          if (counted) return;
          counted = true;
          if (++live === names.length) this.resolveReady();
        };
        void this.pump(name, onLive);
      }
      this.notify();
    })().catch((err) => this.fail(err));
  }

  private async pump(name: string, onLive: () => void): Promise<void> {
    try {
      for await (const ev of this.source(name, onLive, this.ac.signal)) {
        this.queue.push(ev);
        this.notify();
      }
    } catch (err) {
      if (!this.ac.signal.aborted) this.fail(err);
    } finally {
      onLive();
      this.remaining = (this.remaining ?? 1) - 1;
      this.notify();
    }
  }

  private fail(err: unknown): void {
    this.failure ??= { error: err };
    if (!this.isReady) this.rejectReady(err);
    this.notify();
  }

  private notify(): void {
    const w = this.wake;
    this.wake = undefined;
    w?.();
  }
}

/** Finished Turns of one or more Computers. Iterate it with `for await`. */
export class TurnStream implements AsyncIterableIterator<Turn> {
  private readonly builder = new TurnBuilder();
  constructor(private readonly events: EventStream) {}

  /** See {@link EventStream.ready}. */
  ready(): Promise<void> {
    return this.events.ready();
  }

  close(): void {
    this.events.close();
  }

  async next(): Promise<IteratorResult<Turn>> {
    for (;;) {
      const r = await this.events.next();
      if (r.done) return { value: undefined, done: true };
      const turn = this.builder.add(r.value);
      if (turn) return { value: turn, done: false };
    }
  }

  async return(): Promise<IteratorResult<Turn>> {
    this.close();
    return { value: undefined, done: true };
  }

  [Symbol.asyncIterator](): this {
    return this;
  }
}

/** Folds a Computer's events into Turns. */
class TurnBuilder {
  private readonly open = new Map<string, { text: string; afterTool: boolean; messages: TurnInboxMessage[]; tools: Map<string, ToolCall>; startedAt?: string }>();

  add(ev: A12sEvent): Turn | undefined {
    if (!ev.turn) return undefined;
    const name = agentComputerOf(ev);
    const key = `${name}\u0000${ev.turn}`;
    let t = this.open.get(key);
    if (!t) this.open.set(key, (t = { text: "", afterTool: false, messages: [], tools: new Map() }));
    switch (ev.type) {
      case "turn.started":
        t.messages = ev.data.inbox?.messages ?? [];
        t.startedAt = ev.ts;
        return undefined;
      case "tool.started":
        t.afterTool = true;
        t.tools.set(ev.data.id, { id: ev.data.id, name: ev.data.name, input: ev.data.input, isError: false });
        return undefined;
      case "tool.finished": {
        const started = t.tools.get(ev.data.id);
        t.tools.set(ev.data.id, { id: ev.data.id, name: ev.data.name, input: started?.input, output: ev.data.output, isError: ev.data.isError });
        return undefined;
      }
      case "text.delta":
        // Text before and after a tool call are separate messages: keep them on separate lines.
        if (t.afterTool && t.text && !t.text.endsWith("\n")) t.text += "\n";
        t.afterTool = false;
        t.text += ev.data.text;
        return undefined;
      case "turn.finished":
        this.open.delete(key);
        return {
          agentComputer: name,
          computer: name,
          id: ev.turn,
          session: ev.session,
          status: ev.data.status,
          text: t.text,
          ...(ev.data.error ? { error: ev.data.error } : {}),
          messages: t.messages,
          tools: [...t.tools.values()],
          usage: ev.data.usage,
          ...(ev.data.tokens ? { tokens: ev.data.tokens } : {}),
          ...(t.startedAt ? { startedAt: t.startedAt } : {}),
          finishedAt: ev.ts,
        };
    }
    return undefined;
  }
}

/** fetch rejects with a TypeError when the network fails (WHATWG fetch; undici, browsers, Deno, Bun). */
function isNetworkError(err: unknown): boolean {
  return err instanceof TypeError || (err instanceof Error && err.name === "AbortError");
}

function parseJSON(text: string): unknown {
  try {
    return text ? JSON.parse(text) : undefined;
  } catch {
    return text;
  }
}

function parseError(status: number, text: string): ApiErrorBody {
  try {
    const b = JSON.parse(text);
    if (b && typeof b.code === "string") return b;
  } catch {
    // not JSON: fall through
  }
  return { code: `http_${status}`, message: text.trim() || `HTTP ${status}` };
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(t);
        resolve();
      },
      { once: true },
    );
  });
}
