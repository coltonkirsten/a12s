// src/sse.ts
async function* readSSE(body) {
  const decoder = new TextDecoder();
  let buf = "";
  let frame = { data: [] };
  const reader = body.getReader();
  try {
    for (; ; ) {
      const { value, done } = await reader.read();
      if (done) return;
      buf += decoder.decode(value, { stream: true });
      let nl;
      while ((nl = buf.search(/\r\n|\r|\n/)) >= 0) {
        if (buf[nl] === "\r" && nl === buf.length - 1) break;
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + (buf.startsWith("\r\n", nl) ? 2 : 1));
        if (line === "") {
          if (frame.data.length > 0 || frame.event) {
            yield { id: frame.id, event: frame.event, data: frame.data.join("\n") };
          }
          frame = { data: [] };
          continue;
        }
        if (line.startsWith(":")) continue;
        const colon = line.indexOf(":");
        const field = colon < 0 ? line : line.slice(0, colon);
        let val = colon < 0 ? "" : line.slice(colon + 1);
        if (val.startsWith(" ")) val = val.slice(1);
        if (field === "data") frame.data.push(val);
        else if (field === "id") frame.id = val;
        else if (field === "event") frame.event = val;
      }
    }
  } finally {
    reader.releaseLock();
  }
}

// src/client.ts
var A12sError = class extends Error {
  status;
  /** A stable code such as `busy`, `asleep`, `no_grant`, `inbox_full`, `not_found` (see docs/api.md#errors). */
  code;
  fix;
  details;
  constructor(status, body) {
    super(body.fix ? `${body.code}: ${body.message}  fix: ${body.fix}` : `${body.code}: ${body.message}`);
    this.name = "A12sError";
    this.status = status;
    this.code = body.code;
    this.fix = body.fix;
    this.details = body.details;
  }
};
var A12sGapError = class extends Error {
  after;
  data;
  constructor(data) {
    super(`journal gap after seq ${data.after ?? "(none)"}: events were lost; resync with status()`);
    this.name = "A12sGapError";
    this.after = data.after;
    this.data = data;
  }
};
var A12sStreamError = class extends Error {
  constructor(message, options) {
    super(message, options);
    this.name = "A12sStreamError";
  }
};
var enc = encodeURIComponent;
var A12sClient = class {
  url;
  token;
  fetchImpl;
  constructor(opts = {}) {
    this.url = (opts.url ?? "").replace(/\/+$/, "");
    this.token = opts.token;
    this.fetchImpl = opts.fetch ?? ((...args) => fetch(...args));
  }
  headers(extra = {}) {
    return this.token ? { authorization: `Bearer ${this.token}`, ...extra } : extra;
  }
  async request(method, path, body, signal) {
    const res = await this.fetchImpl(this.url + path, {
      method,
      headers: this.headers(body === void 0 ? {} : { "content-type": "application/json" }),
      body: body === void 0 ? void 0 : JSON.stringify(body),
      signal
    });
    const text = await res.text();
    if (!res.ok) throw new A12sError(res.status, parseError(res.status, text));
    return text ? JSON.parse(text) : void 0;
  }
  /** Every Computer and Service: phase, session, active Turn, inbox, power. */
  status() {
    return this.request("GET", "/v1/status");
  }
  async computer(name) {
    const c = (await this.status()).computers.find((x) => x.name === name);
    if (!c) throw new A12sError(404, { code: "not_found", message: `no Computer ${name}` });
    return c;
  }
  /**
   * Puts a Message in a Computer's inbox and returns at once. It is delivered at the Computer's next idle moment,
   * batched with anything else pending. Works while the Computer is busy or asleep.
   */
  send(computer, msg) {
    const m = typeof msg === "string" ? { body: msg } : { ...msg };
    m.idempotencyKey ??= globalThis.crypto.randomUUID();
    return this.request("POST", `/v1/computers/${enc(computer)}/inbox`, m);
  }
  /** Sends a Message, waits for the Turn that delivers it and resolves when that Turn ends: what `a12s prompt` does. */
  async prompt(computer, body, opts = {}) {
    const { signal, onEvent, ...msg } = opts;
    const message = await this.send(computer, { ...msg, body });
    const turn = await this.deliveredIn(computer, message.id, signal);
    const builder = new TurnBuilder();
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
  events(computers, opts = {}) {
    const names = typeof computers === "string" ? [computers] : computers;
    if ((opts.turn !== void 0 || opts.after !== void 0) && names?.length !== 1) {
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
  turns(computers, opts = {}) {
    return new TurnStream(this.events(computers, opts));
  }
  inbox(computer, opts = {}) {
    const q = new URLSearchParams();
    if (opts.state) q.set("state", opts.state);
    if (opts.bodies) q.set("bodies", "true");
    if (opts.after !== void 0) q.set("after", String(opts.after));
    const qs = q.toString();
    return this.request("GET", `/v1/computers/${enc(computer)}/inbox${qs ? "?" + qs : ""}`);
  }
  message(computer, id) {
    return this.request("GET", `/v1/computers/${enc(computer)}/inbox/${enc(id)}`);
  }
  /** Withdraws a pending Message; a 409 A12sError names its state if it is too late. */
  withdraw(computer, id) {
    return this.request("DELETE", `/v1/computers/${enc(computer)}/inbox/${enc(id)}`);
  }
  /** Interrupts a Turn (default: the active one). The Turn ends `interrupted`; nothing is replayed. */
  async stop(computer, turn) {
    const id = turn ?? (await this.computer(computer)).activeTurn;
    if (id) await this.request("POST", `/v1/computers/${enc(computer)}/turns/${enc(id)}/stop`, {});
  }
  /** Stops the Computer's processes and keeps its disks, session and inbox. */
  async sleep(computer) {
    await this.request("POST", `/v1/computers/${enc(computer)}/power`, { state: "asleep" });
  }
  /** Starts a sleeping Computer; its session resumes on the next Turn. */
  async wake(computer) {
    await this.request("POST", `/v1/computers/${enc(computer)}/power`, { state: "awake" });
  }
  async services() {
    return (await this.request("GET", "/v1/services")).services ?? [];
  }
  service(name) {
    return this.request("GET", `/v1/services/${enc(name)}`);
  }
  /** A scoped token for another program; owner only. The secret is in the response once. */
  createToken(req) {
    return this.request("POST", "/v1/tokens", req);
  }
  /** What `a12s up` sends: validate and apply a whole Fleet. Most apps leave this to the CLI. */
  putFleet(req) {
    return this.request("PUT", "/v1/fleet", req);
  }
  /** Waits until the Message is delivered and returns the Turn that carries it. */
  async deliveredIn(computer, id, signal) {
    while (!signal?.aborted) {
      for await (const ev of this.stream(computer, `/v1/computers/${enc(computer)}/inbox/${enc(id)}/events`, void 0, {
        signal,
        once: true
      })) {
        const d = ev.data ?? {};
        const type = ev.type;
        const state = type === "snapshot" ? d.message?.state : type;
        const turn = type === "snapshot" ? d.message?.turn : d.turn;
        if (state === "delivered" && turn) return turn;
        if (["withdrawn", "discarded", "expired", "replaced"].includes(state)) {
          throw new A12sError(409, { code: state, message: `message ${id} was ${state} before delivery` });
        }
        if (type === "snapshot" && d.held || type === "held") {
          throw new A12sError(409, {
            code: "held",
            message: `${computer}'s inbox is held (${d.holdReason ?? d.reason}); ${id} waits in the inbox`,
            fix: `a12s inbox resume ${computer}`
          });
        }
      }
      await sleep(1e3, signal);
    }
    throw signal.reason ?? new Error("aborted");
  }
  async *stream(computer, path, after, opts) {
    let cursor = after;
    for (; ; ) {
      if (opts.signal?.aborted) return;
      let res;
      try {
        res = await this.fetchImpl(this.url + path, {
          headers: this.headers({
            accept: "text/event-stream",
            ...cursor !== void 0 ? { "last-event-id": String(cursor) } : {}
          }),
          signal: opts.signal
        });
      } catch (err) {
        if (opts.signal?.aborted) return;
        if (!isNetworkError(err)) throw err;
        await sleep(opts.reconnectMs ?? 1e3, opts.signal);
        continue;
      }
      if (!res.ok) {
        const err = new A12sError(res.status, parseError(res.status, await res.text()));
        if (res.status !== 503 && res.status !== 502) throw err;
        opts.live?.();
        await sleep(opts.reconnectMs ?? 1e3, opts.signal);
        continue;
      }
      if (!res.body) throw new A12sStreamError(`event stream ${path}: response has no body`);
      const frames = readSSE(res.body);
      try {
        for (; ; ) {
          let r;
          try {
            r = await frames.next();
          } catch {
            break;
          }
          if (r.done) break;
          const f = r.value;
          if (f.event === "head") {
            const seq = parseJSON(f.data)?.seq;
            if (typeof seq === "number") cursor = Math.max(cursor ?? 0, seq);
            opts.live?.();
            continue;
          }
          if (f.event === "gap") {
            const server = parseJSON(f.data);
            const data = { ...server && typeof server === "object" ? server : {}, after: cursor };
            cursor = void 0;
            if (opts.onGap === "throw") throw new A12sGapError(data);
            const gap = { v: 1, computer, session: "", turn: "", seq: 0, ts: (/* @__PURE__ */ new Date()).toISOString(), type: "gap", data };
            yield gap;
            continue;
          }
          if (f.event || !f.data) continue;
          let ev;
          try {
            ev = JSON.parse(f.data);
          } catch (err) {
            throw new A12sStreamError(`malformed event after seq ${cursor ?? "(start)"}: ${f.data.slice(0, 120)}`, {
              cause: err
            });
          }
          if (f.id !== void 0) cursor = Number(f.id);
          yield ev;
        }
      } finally {
        await frames.return(void 0);
        res.body.cancel().catch(() => {
        });
      }
      if (opts.signal?.aborted) return;
      if (opts.once) return;
      await sleep(opts.reconnectMs ?? 1e3, opts.signal);
    }
  }
};
var EventStream = class {
  constructor(source, names, signal) {
    this.source = source;
    this.names = names;
    this.readyP = new Promise((resolve, reject) => {
      this.resolveReady = () => {
        this.isReady = true;
        resolve();
      };
      this.rejectReady = reject;
    });
    this.readyP.catch(() => {
    });
    if (signal?.aborted) this.ac.abort();
    signal?.addEventListener("abort", () => this.close(), { once: true });
  }
  ac = new AbortController();
  queue = [];
  started;
  remaining;
  failure;
  wake;
  readyP;
  resolveReady;
  rejectReady;
  isReady = false;
  /**
   * Resolves once every stream is connected and live (a sleeping Computer counts: its events start when it wakes).
   * Events that happen after this are never missed.
   */
  ready() {
    this.start();
    return this.readyP;
  }
  close() {
    this.ac.abort();
    this.resolveReady();
    this.notify();
  }
  async next() {
    this.start();
    for (; ; ) {
      if (this.queue.length) return { value: this.queue.shift(), done: false };
      if (this.failure) {
        const { error } = this.failure;
        this.failure = void 0;
        this.close();
        throw error;
      }
      if (this.ac.signal.aborted || this.remaining === 0) return { value: void 0, done: true };
      await new Promise((r) => this.wake = r);
    }
  }
  async return() {
    this.close();
    return { value: void 0, done: true };
  }
  async throw(err) {
    this.close();
    throw err;
  }
  [Symbol.asyncIterator]() {
    return this;
  }
  start() {
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
  async pump(name, onLive) {
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
  fail(err) {
    this.failure ??= { error: err };
    if (!this.isReady) this.rejectReady(err);
    this.notify();
  }
  notify() {
    const w = this.wake;
    this.wake = void 0;
    w?.();
  }
};
var TurnStream = class {
  constructor(events) {
    this.events = events;
  }
  builder = new TurnBuilder();
  /** See {@link EventStream.ready}. */
  ready() {
    return this.events.ready();
  }
  close() {
    this.events.close();
  }
  async next() {
    for (; ; ) {
      const r = await this.events.next();
      if (r.done) return { value: void 0, done: true };
      const turn = this.builder.add(r.value);
      if (turn) return { value: turn, done: false };
    }
  }
  async return() {
    this.close();
    return { value: void 0, done: true };
  }
  [Symbol.asyncIterator]() {
    return this;
  }
};
var TurnBuilder = class {
  open = /* @__PURE__ */ new Map();
  add(ev) {
    if (!ev.turn) return void 0;
    const key = `${ev.computer}\0${ev.turn}`;
    let t = this.open.get(key);
    if (!t) this.open.set(key, t = { text: "", afterTool: false, messages: [] });
    switch (ev.type) {
      case "turn.started":
        t.messages = ev.data.inbox?.messages ?? [];
        t.startedAt = ev.ts;
        return void 0;
      case "tool.started":
        t.afterTool = true;
        return void 0;
      case "text.delta":
        if (t.afterTool && t.text && !t.text.endsWith("\n")) t.text += "\n";
        t.afterTool = false;
        t.text += ev.data.text;
        return void 0;
      case "turn.finished":
        this.open.delete(key);
        return {
          computer: ev.computer,
          id: ev.turn,
          session: ev.session,
          status: ev.data.status,
          text: t.text,
          ...ev.data.error ? { error: ev.data.error } : {},
          messages: t.messages,
          usage: ev.data.usage,
          ...t.startedAt ? { startedAt: t.startedAt } : {},
          finishedAt: ev.ts
        };
    }
    return void 0;
  }
};
function isNetworkError(err) {
  return err instanceof TypeError || err instanceof Error && err.name === "AbortError";
}
function parseJSON(text) {
  try {
    return text ? JSON.parse(text) : void 0;
  } catch {
    return text;
  }
}
function parseError(status, text) {
  try {
    const b = JSON.parse(text);
    if (b && typeof b.code === "string") return b;
  } catch {
  }
  return { code: `http_${status}`, message: text.trim() || `HTTP ${status}` };
}
function sleep(ms, signal) {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(t);
        resolve();
      },
      { once: true }
    );
  });
}

// src/index.ts
async function connect(opts = {}) {
  return new A12sClient(opts);
}
export {
  A12sClient,
  A12sError,
  A12sGapError,
  A12sStreamError,
  EventStream,
  TurnStream,
  connect
};
