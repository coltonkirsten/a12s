// Runs in Node, Deno, Bun and browsers: no node: imports here (`npm run check:browser` enforces it).
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

export interface PromptOptions extends Omit<EnqueueRequest, "body"> {
  signal?: AbortSignal;
  /** Every event of the Turn, as it streams. */
  onEvent?: (ev: A12sEvent) => void;
}

/** What `prompt()` resolves with: the finished Turn and the Message that started it. */
export interface PromptResult extends Turn {
  message: EnqueueResponse;
}

const enc = encodeURIComponent;

export class A12sClient {
  readonly url: string;
  private readonly token?: string;
  private readonly fetchImpl: typeof fetch;

  constructor(opts: A12sClientOptions = {}) {
    this.url = (opts.url ?? "").replace(/\/+$/, "");
    this.token = opts.token;
    this.fetchImpl = opts.fetch ?? ((...args) => fetch(...args));
  }

  private headers(extra: Record<string, string> = {}): Record<string, string> {
    return this.token ? { authorization: `Bearer ${this.token}`, ...extra } : extra;
  }

  private async request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    const res = await this.fetchImpl(this.url + path, {
      method,
      headers: this.headers(body === undefined ? {} : { "content-type": "application/json" }),
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
    const text = await res.text();
    if (!res.ok) throw new A12sError(res.status, parseError(res.status, text));
    return (text ? JSON.parse(text) : undefined) as T;
  }

  /** Every Computer and Service: phase, session, active Turn, inbox, power. */
  status(): Promise<StatusResponse> {
    return this.request("GET", "/v1/status");
  }

  async computer(name: string): Promise<ComputerStatus> {
    const c = (await this.status()).computers.find((x) => x.name === name);
    if (!c) throw new A12sError(404, { code: "not_found", message: `no Computer ${name}` });
    return c;
  }

  /**
   * Puts a Message in a Computer's inbox and returns at once. It is delivered at the Computer's next idle moment,
   * batched with anything else pending. Works while the Computer is busy or asleep.
   */
  send(computer: string, msg: EnqueueRequest | string): Promise<EnqueueResponse> {
    const m: EnqueueRequest = typeof msg === "string" ? { body: msg } : { ...msg };
    m.idempotencyKey ??= globalThis.crypto.randomUUID();
    return this.request("POST", `/v1/computers/${enc(computer)}/inbox`, m);
  }

  /** Sends a Message, waits for the Turn that delivers it and resolves when that Turn ends: what `a12s prompt` does. */
  async prompt(computer: string, body: string, opts: PromptOptions = {}): Promise<PromptResult> {
    const { signal, onEvent, ...msg } = opts;
    const message = await this.send(computer, { ...msg, body });
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
      throw new TypeError("events: turn and after need exactly one Computer");
    }
    return new EventStream((name, live, signal) => {
      const q = opts.turn ? `?turn=${enc(opts.turn)}` : "";
      return this.stream(name, `/v1/computers/${enc(name)}/events${q}`, opts.after, { ...opts, signal, live });
    }, names ?? (async () => (await this.status()).computers.map((c) => c.name)), opts.signal);
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
    return this.request("GET", `/v1/computers/${enc(computer)}/inbox${qs ? "?" + qs : ""}`);
  }

  message(computer: string, id: string): Promise<Message> {
    return this.request("GET", `/v1/computers/${enc(computer)}/inbox/${enc(id)}`);
  }

  /** Withdraws a pending Message; a 409 A12sError names its state if it is too late. */
  withdraw(computer: string, id: string): Promise<Message> {
    return this.request("DELETE", `/v1/computers/${enc(computer)}/inbox/${enc(id)}`);
  }

  /** Interrupts a Turn (default: the active one). The Turn ends `interrupted`; nothing is replayed. */
  async stop(computer: string, turn?: string): Promise<void> {
    const id = turn ?? (await this.computer(computer)).activeTurn;
    if (id) await this.request("POST", `/v1/computers/${enc(computer)}/turns/${enc(id)}/stop`, {});
  }

  /** Stops the Computer's processes and keeps its disks, session and inbox. */
  async sleep(computer: string): Promise<void> {
    await this.request("POST", `/v1/computers/${enc(computer)}/power`, { state: "asleep" });
  }

  /** Starts a sleeping Computer; its session resumes on the next Turn. */
  async wake(computer: string): Promise<void> {
    await this.request("POST", `/v1/computers/${enc(computer)}/power`, { state: "awake" });
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
      for await (const ev of this.stream(computer, `/v1/computers/${enc(computer)}/inbox/${enc(id)}/events`, undefined, {
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
        const err = new A12sError(res.status, parseError(res.status, await res.text()));
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

type Source = (computer: string, live: () => void, signal: AbortSignal) => AsyncGenerator<A12sEvent>;

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
  private readonly open = new Map<string, { text: string; afterTool: boolean; messages: TurnInboxMessage[]; startedAt?: string }>();

  add(ev: A12sEvent): Turn | undefined {
    if (!ev.turn) return undefined;
    const key = `${ev.computer}\u0000${ev.turn}`;
    let t = this.open.get(key);
    if (!t) this.open.set(key, (t = { text: "", afterTool: false, messages: [] }));
    switch (ev.type) {
      case "turn.started":
        t.messages = ev.data.inbox?.messages ?? [];
        t.startedAt = ev.ts;
        return undefined;
      case "tool.started":
        t.afterTool = true;
        return undefined;
      case "text.delta":
        // Text before and after a tool call are separate messages: keep them on separate lines.
        if (t.afterTool && t.text && !t.text.endsWith("\n")) t.text += "\n";
        t.afterTool = false;
        t.text += ev.data.text;
        return undefined;
      case "turn.finished":
        this.open.delete(key);
        return {
          computer: ev.computer,
          id: ev.turn,
          session: ev.session,
          status: ev.data.status,
          text: t.text,
          ...(ev.data.error ? { error: ev.data.error } : {}),
          messages: t.messages,
          usage: ev.data.usage,
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
