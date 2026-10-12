// The root entry runs in Node, Deno, Bun and browsers: no node: imports here. Node-only helpers (fromFleet) live in
// ./node.ts, exported as "@coltonkirsten/a12s-client/node". `npm run check:browser` enforces it.
import { readSSE } from "./sse.js";
import type {
  A12sEvent,
  ApiErrorBody,
  ComputerStatus,
  EnqueueRequest,
  EnqueueResponse,
  InboxListOptions,
  InboxListResponse,
  Message,
  PutFleetRequest,
  PutFleetResponse,
  ServiceStatus,
  StatusResponse,
  Token,
  TokenRequest,
  TurnFinishedData,
} from "./types.js";

export * from "./types.js";
export { readSSE } from "./sse.js";

export interface A12sClientOptions {
  /** The Controller's owner API, e.g. http://127.0.0.1:52011 (the port is in ~/.a12s/<fleet>/controller.json). */
  url: string;
  /** The owner token from controller.json, or a scoped token from `a12s token create`. */
  token: string;
  fetch?: typeof fetch;
}

/** A refused call: the HTTP status and the Controller's error, including its fix line. */
export class A12sError extends Error {
  readonly status: number;
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

export interface EventsOptions {
  /** Replay one Turn from its start, then follow it. */
  turn?: string;
  /** Resume after this journal seq. Without `turn` or `after`, the stream starts live. */
  after?: number;
  signal?: AbortSignal;
  /** Delay before reconnecting a dropped stream. Default 1000 ms. */
  reconnectMs?: number;
  /**
   * What to do when the Controller answers the cursor with `event: gap` (the events after it are no longer, or not
   * yet, in the journal). "yield" (default) yields a {@link GapEvent} and carries on live; "throw" throws
   * {@link A12sGapError}. Either way, resync from `status()` / `inbox()`: events were lost.
   */
  onGap?: "yield" | "throw";
}

/** The data of a {@link GapEvent}. */
export interface GapData {
  /** The cursor the stream asked to resume after, if any. */
  after?: number;
  /** The Controller's last journal seq when it answered, if it said. */
  head?: number;
  lastEventId?: string;
}

/**
 * Yielded by `events()` (type "gap", seq 0) when the journal could not resume from the cursor: the events after
 * `data.after` were lost. The stream continues live after it; resync state with `status()` and `inbox()`.
 */
export type GapEvent = A12sEvent<GapData> & { type: "gap" };

/** Thrown by `events(..., { onGap: "throw" })` when events were lost. */
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

export interface PromptResult {
  message: EnqueueResponse;
  turn: string;
  status: TurnFinishedData["status"];
  /** The concatenated text of the Turn. */
  text: string;
  error?: string;
}

export interface PromptOptions extends Omit<EnqueueRequest, "body"> {
  signal?: AbortSignal;
  onEvent?: (ev: A12sEvent) => void;
}

const enc = encodeURIComponent;

export class A12sClient {
  readonly url: string;
  private readonly token: string;
  private readonly fetchImpl: typeof fetch;

  constructor(opts: A12sClientOptions) {
    this.url = opts.url.replace(/\/+$/, "");
    this.token = opts.token;
    this.fetchImpl = opts.fetch ?? fetch;
  }

  private async request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    const res = await this.fetchImpl(this.url + path, {
      method,
      headers: {
        authorization: `Bearer ${this.token}`,
        ...(body === undefined ? {} : { "content-type": "application/json" }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
    const text = await res.text();
    if (!res.ok) throw new A12sError(res.status, parseError(res.status, text));
    return (text ? JSON.parse(text) : undefined) as T;
  }

  /** `a12s up` without the CLI: validate and apply a whole Fleet. Most apps leave this to the CLI. */
  putFleet(req: PutFleetRequest): Promise<PutFleetResponse> {
    return this.request("PUT", "/v1/fleet", req);
  }

  status(): Promise<StatusResponse> {
    return this.request("GET", "/v1/status");
  }

  async computer(name: string): Promise<ComputerStatus> {
    const st = await this.status();
    const c = st.computers.find((x) => x.name === name);
    if (!c) throw new A12sError(404, { code: "not_found", message: `no Computer ${name}` });
    return c;
  }

  /** Enqueues a Message. It is delivered at the Computer's next idle moment, batched with anything else pending. */
  send(computer: string, msg: EnqueueRequest | string): Promise<EnqueueResponse> {
    const m: EnqueueRequest = typeof msg === "string" ? { body: msg } : { ...msg };
    m.idempotencyKey ??= globalThis.crypto.randomUUID();
    return this.request("POST", `/v1/computers/${enc(computer)}/inbox`, m);
  }

  /** Alias of send. */
  enqueue(computer: string, msg: EnqueueRequest | string): Promise<EnqueueResponse> {
    return this.send(computer, msg);
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

  stop(computer: string, turn: string): Promise<unknown> {
    return this.request("POST", `/v1/computers/${enc(computer)}/turns/${enc(turn)}/stop`, {});
  }

  async services(): Promise<ServiceStatus[]> {
    return (await this.request<{ services: ServiceStatus[] }>("GET", "/v1/services")).services ?? [];
  }

  service(name: string): Promise<ServiceStatus> {
    return this.request("GET", `/v1/services/${enc(name)}`);
  }

  createToken(req: TokenRequest): Promise<Token> {
    return this.request("POST", "/v1/tokens", req);
  }

  /**
   * A Computer's journal as it happens. Reconnects with Last-Event-ID after a network drop, so nothing is missed or
   * repeated. Ends when `signal` aborts or the consumer breaks out of the loop; throws on an API error other than
   * 502/503, on a malformed event, and (with `onGap: "throw"`) on a gap. If the cursor is no longer in the journal it
   * yields a {@link GapEvent} first: narrow on `ev.type`.
   */
  events(computer: string, opts: EventsOptions = {}): AsyncGenerator<A12sEvent> {
    const q = opts.turn ? `?turn=${enc(opts.turn)}` : "";
    return this.stream(computer, `/v1/computers/${enc(computer)}/events${q}`, opts.after, opts);
  }

  /** State changes of one Message: a snapshot first, then claimed, delivered (with its turn), returned, held… */
  messageEvents(computer: string, id: string, opts: { signal?: AbortSignal } = {}): AsyncGenerator<A12sEvent> {
    return this.stream(computer, `/v1/computers/${enc(computer)}/inbox/${enc(id)}/events`, undefined, {
      ...opts,
      once: true,
    });
  }

  /** Waits until the Message is delivered and returns the Turn that carries it. */
  async waitForTurn(computer: string, id: string, signal?: AbortSignal): Promise<string> {
    // A fresh subscription starts with a snapshot, so resubscribing after a drop never misses the delivery.
    for (let attempt = 0; attempt < 30 && !signal?.aborted; attempt++) {
      const turn = await this.waitForTurnOnce(computer, id, signal);
      if (turn) return turn;
      await sleep(1000, signal);
    }
    throw new Error(`gave up waiting for ${computer} to take message ${id}`);
  }

  private async waitForTurnOnce(computer: string, id: string, signal?: AbortSignal): Promise<string | undefined> {
    for await (const ev of this.messageEvents(computer, id, { signal })) {
      const d = (ev.data ?? {}) as any;
      const state: string = ev.type === "snapshot" ? d.message?.state : ev.type;
      const turn: string | undefined = ev.type === "snapshot" ? d.message?.turn : d.turn;
      if (state === "delivered" && turn) return turn;
      if (["withdrawn", "discarded", "expired", "replaced"].includes(state)) {
        throw new A12sError(409, { code: state, message: `message ${id} was ${state} before delivery` });
      }
      if ((ev.type === "snapshot" && d.held) || ev.type === "held") {
        const reason = d.holdReason ?? d.reason;
        throw new A12sError(409, {
          code: "held",
          message: `${computer}'s inbox is held (${reason}); ${id} waits in the inbox`,
          fix: `a12s inbox resume ${computer}`,
        });
      }
    }
    return undefined;
  }

  /** Enqueue, wait for delivery, and stream the Turn to its end: what `a12s prompt` does. */
  async prompt(computer: string, body: string, opts: PromptOptions = {}): Promise<PromptResult> {
    const { signal, onEvent, ...msg } = opts;
    const message = await this.send(computer, { ...msg, body });
    const turn = await this.waitForTurn(computer, message.id, signal);
    let text = "";
    // A gap mid-Turn would silently drop text: fail loudly instead.
    for await (const ev of this.events(computer, { turn, signal, onGap: "throw" })) {
      onEvent?.(ev);
      if (ev.type === "text.delta") text += (ev.data as { text: string }).text;
      if (ev.type === "turn.finished" && ev.turn === turn) {
        const d = ev.data as TurnFinishedData;
        return { message, turn, status: d.status, text, error: d.error };
      }
    }
    throw new Error(`event stream for turn ${turn} ended early`);
  }

  private async *stream(
    computer: string,
    path: string,
    after: number | undefined,
    opts: { signal?: AbortSignal; reconnectMs?: number; once?: boolean; onGap?: "yield" | "throw" },
  ): AsyncGenerator<A12sEvent> {
    let cursor = after;
    for (;;) {
      if (opts.signal?.aborted) return;
      let res: Response;
      try {
        res = await this.fetchImpl(this.url + path, {
          headers: {
            authorization: `Bearer ${this.token}`,
            accept: "text/event-stream",
            ...(cursor !== undefined ? { "last-event-id": String(cursor) } : {}),
          },
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
    signal?.addEventListener("abort", () => {
      clearTimeout(t);
      resolve();
    }, { once: true });
  });
}
