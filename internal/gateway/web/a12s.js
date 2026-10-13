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
var NotFound = class extends A12sError {
  constructor(status, body) {
    super(status, body);
    this.name = "NotFound";
  }
};
function apiError(status, body) {
  return status === 404 ? new NotFound(status, body) : new A12sError(status, body);
}
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
  shared;
  constructor(opts = {}) {
    this.url = (opts.url ?? "").replace(/\/+$/, "");
    this.token = opts.token;
    this.fetchImpl = opts.fetch ?? ((...args) => fetch(...args));
  }
  headers(extra = {}) {
    return this.token ? { authorization: `Bearer ${this.token}`, ...extra } : extra;
  }
  /** @internal */
  async request(method, path, body, opts = {}) {
    const res = await this.fetchImpl(this.url + path, {
      method,
      headers: this.headers({ ...body === void 0 ? {} : { "content-type": "application/json" }, ...opts.headers }),
      body: body === void 0 ? void 0 : JSON.stringify(body),
      signal: opts.signal
    });
    const text = await res.text();
    if (!res.ok) throw apiError(res.status, parseError(res.status, text));
    return text ? JSON.parse(text) : void 0;
  }
  /** Every Computer and Service: phase, session, active Turn, inbox, power. */
  status() {
    return this.request("GET", "/v1/status");
  }
  async computer(name) {
    const c = (await this.status()).agentComputers.find((x) => x.name === name);
    if (!c) throw new NotFound(404, { code: "not_found", message: `no agent computer ${name}` });
    return c;
  }
  /**
   * Puts a Message in a Computer's inbox and returns at once. It is delivered at the Computer's next idle moment,
   * batched with anything else pending. Works while the Computer is busy or asleep.
   */
  send(computer, msg) {
    const m = typeof msg === "string" ? { body: msg } : { ...msg };
    m.idempotencyKey ??= globalThis.crypto.randomUUID();
    return this.request("POST", `/v1/agent-computers/${enc(computer)}/inbox`, m);
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
  sharedEvents(names, opts = {}) {
    this.shared ??= new SharedEvents(
      (cursor, signal) => this.fetchImpl(this.url + "/v1/events", {
        headers: this.headers({ accept: "text/event-stream", ...cursor !== void 0 ? { "last-event-id": cursor } : {} }),
        signal
      })
    );
    const shared = this.shared;
    const wanted2 = names && new Set(names);
    return new EventStream(
      (_name, live, signal) => shared.subscribe({ wanted: wanted2, live, onGap: opts.onGap, reconnectMs: opts.reconnectMs }, signal),
      [""],
      opts.signal
    );
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
    return this.request("GET", `/v1/agent-computers/${enc(computer)}/inbox${qs ? "?" + qs : ""}`);
  }
  message(computer, id) {
    return this.request("GET", `/v1/agent-computers/${enc(computer)}/inbox/${enc(id)}`);
  }
  /** Withdraws a pending Message; a 409 A12sError names its state if it is too late. */
  withdraw(computer, id) {
    return this.request("DELETE", `/v1/agent-computers/${enc(computer)}/inbox/${enc(id)}`);
  }
  /** Interrupts a Turn (default: the active one). The Turn ends `interrupted`; nothing is replayed. */
  async stop(computer, turn) {
    const id = turn ?? (await this.computer(computer)).activeTurn;
    if (id) await this.request("POST", `/v1/agent-computers/${enc(computer)}/turns/${enc(id)}/stop`, {});
  }
  /**
   * Stops the agent computer's processes and keeps its disks, session and inbox. It stays asleep until wake(); with
   * `allowWake` its power policy (wakeOnMessage) may wake it instead.
   */
  async sleep(computer, opts = {}) {
    await this.request("POST", `/v1/agent-computers/${enc(computer)}/power`, {
      state: "asleep",
      ...opts.allowWake ? { allowWake: true } : {}
    });
  }
  /** Starts a sleeping agent computer; its session resumes on the next Turn. `clearInbox` discards pending Messages. */
  async wake(computer, opts = {}) {
    await this.request("POST", `/v1/agent-computers/${enc(computer)}/power`, {
      state: "awake",
      ...opts.clearInbox ? { clearInbox: true, requestId: crypto.randomUUID() } : {}
    });
  }
  /** Holds the inbox: Messages still queue, none is delivered until resume(). */
  hold(computer, opts = {}) {
    return this.request("POST", `/v1/agent-computers/${enc(computer)}/inbox/hold`, opts.reason ? { reason: opts.reason } : {});
  }
  /** Releases a hold; pending Messages are delivered at the next idle moment. */
  resume(computer) {
    return this.request("POST", `/v1/agent-computers/${enc(computer)}/inbox/resume`, {});
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
      for await (const ev of this.stream(computer, `/v1/agent-computers/${enc(computer)}/inbox/${enc(id)}/events`, void 0, {
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
        const err = apiError(res.status, parseError(res.status, await res.text()));
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
var SharedEvents = class {
  constructor(open) {
    this.open = open;
  }
  subs = /* @__PURE__ */ new Set();
  conn;
  live = false;
  async *subscribe(sub, signal) {
    const queue = [];
    let failure;
    let wake;
    const notify = () => {
      const w = wake;
      wake = void 0;
      w?.();
    };
    const s = {
      ...sub,
      push: (ev) => {
        queue.push(ev);
        notify();
      },
      fail: (error) => {
        failure ??= { error };
        notify();
      }
    };
    signal.addEventListener("abort", notify, { once: true });
    this.add(s);
    try {
      for (; ; ) {
        if (queue.length) {
          yield queue.shift();
          continue;
        }
        if (failure) throw failure.error;
        if (signal.aborted) return;
        await new Promise((r) => wake = r);
      }
    } finally {
      signal.removeEventListener("abort", notify);
      this.remove(s);
    }
  }
  add(s) {
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
  remove(s) {
    this.subs.delete(s);
    if (this.subs.size === 0) this.stop();
  }
  stop() {
    this.conn?.abort();
    this.conn = void 0;
    this.live = false;
  }
  async run(conn) {
    const { signal } = conn;
    let cursor;
    try {
      while (!signal.aborted) {
        let res;
        try {
          res = await this.open(cursor, signal);
        } catch (err) {
          if (signal.aborted) return;
          if (!isNetworkError(err)) throw err;
        }
        if (res && !res.ok) {
          const err = apiError(res.status, parseError(res.status, await res.text()));
          if (res.status !== 502 && res.status !== 503) throw err;
          res = void 0;
        }
        if (res) {
          if (!res.body) throw new A12sStreamError("GET /v1/events: response has no body");
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
              cursor = this.frame(r.value, cursor);
            }
          } finally {
            await frames.return(void 0);
            res.body.cancel().catch(() => {
            });
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
  frame(f, cursor) {
    if (f.event === "head") {
      const id = parseJSON(f.data)?.id;
      this.live = true;
      for (const s of this.subs) s.live();
      return typeof id === "string" && id ? id : cursor;
    }
    if (f.event === "gap") {
      const data = cursor !== void 0 ? { lastEventId: cursor } : {};
      const gap = { v: 1, computer: "", session: "", turn: "", seq: 0, ts: (/* @__PURE__ */ new Date()).toISOString(), type: "gap", data };
      for (const s of this.subs) {
        if (s.onGap === "throw") s.fail(new A12sGapError(data));
        else s.push(gap);
      }
      return cursor;
    }
    if (f.event || !f.data) return cursor;
    let ev;
    try {
      ev = JSON.parse(f.data);
    } catch (err) {
      throw new A12sStreamError(`malformed event after ${cursor ?? "(start)"}: ${f.data.slice(0, 120)}`, { cause: err });
    }
    for (const s of this.subs) if (wanted(s.wanted, ev)) s.push(ev);
    return f.id ?? cursor;
  }
  reconnectMs() {
    let ms = 1e3;
    for (const s of this.subs) if (s.reconnectMs !== void 0) ms = Math.min(ms, s.reconnectMs);
    return ms;
  }
};
function agentComputerOf(ev) {
  return ev.agentComputer || ev.computer;
}
function wanted(names, ev) {
  if (!names) return true;
  if (ev.type === "fleet.changed") return (ev.data?.names ?? []).some((n) => names.has(n));
  if (ev.type === "service.phase") return names.has(ev.data?.service);
  return names.has(agentComputerOf(ev));
}
async function* untilTurnFinished(events) {
  try {
    for await (const ev of events) {
      yield ev;
      if (ev.type === "turn.finished") return;
    }
  } finally {
    await events.return(void 0);
  }
}
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
    const name = agentComputerOf(ev);
    const key = `${name}\0${ev.turn}`;
    let t = this.open.get(key);
    if (!t) this.open.set(key, t = { text: "", afterTool: false, messages: [], tools: /* @__PURE__ */ new Map() });
    switch (ev.type) {
      case "turn.started":
        t.messages = ev.data.inbox?.messages ?? [];
        t.startedAt = ev.ts;
        return void 0;
      case "tool.started":
        t.afterTool = true;
        t.tools.set(ev.data.id, { id: ev.data.id, name: ev.data.name, input: ev.data.input, isError: false });
        return void 0;
      case "tool.finished": {
        const started = t.tools.get(ev.data.id);
        t.tools.set(ev.data.id, { id: ev.data.id, name: ev.data.name, input: started?.input, output: ev.data.output, isError: ev.data.isError });
        return void 0;
      }
      case "text.delta":
        if (t.afterTool && t.text && !t.text.endsWith("\n")) t.text += "\n";
        t.afterTool = false;
        t.text += ev.data.text;
        return void 0;
      case "turn.finished":
        this.open.delete(key);
        return {
          agentComputer: name,
          computer: name,
          id: ev.turn,
          session: ev.session,
          status: ev.data.status,
          text: t.text,
          ...ev.data.error ? { error: ev.data.error } : {},
          messages: t.messages,
          tools: [...t.tools.values()],
          usage: ev.data.usage,
          ...ev.data.tokens ? { tokens: ev.data.tokens } : {},
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

// src/fleet.ts
var enc2 = encodeURIComponent;
var maxReplaceTries = 5;
var Fleet = class _Fleet {
  /** The lower-level client the handles use, for API calls that have no method here. */
  client;
  agentComputers;
  services;
  tokens;
  credentials;
  /** No discovery: `url` and `token` as given, by default the page's own origin. */
  constructor(opts = {}) {
    this.client = new A12sClient(opts);
    this.agentComputers = new AgentComputers(this.client);
    this.services = new Services(this.client);
    this.tokens = new Tokens(this.client);
    this.credentials = new Credentials(this.client);
  }
  /**
   * In a browser: the Fleet behind the page's own origin, which is `a12s gateway` serving the page (no token
   * needed). In Node: found like {@link connect} finds it. Pass `url` and `token` to reach anything else.
   */
  static async connect(opts = {}) {
    return new _Fleet(opts);
  }
  get url() {
    return this.client.url;
  }
  async get() {
    const r = await this.client.request("GET", "/v1/fleet");
    return { version: r.version, spec: r.fleet ?? emptySpec(), drift: r.drift ?? [] };
  }
  /**
   * Reads the Fleet, lets `fn` change it (in place, or by returning a new spec) and writes it back only if nobody
   * wrote in between; otherwise it reads again and retries, up to 5 times. `spec.defaults` is always present.
   */
  async replace(fn) {
    for (let attempt = 1; ; attempt++) {
      const cur = await this.get();
      cur.spec.defaults ??= {};
      const next = await fn(cur.spec) ?? cur.spec;
      try {
        warnWrites(
          await this.client.request("PUT", "/v1/fleet", { fleet: next }, {
            headers: { "if-match": `"${cur.version}"` }
          })
        );
      } catch (err) {
        if (attempt < maxReplaceTries && err instanceof A12sError && err.code === "version_mismatch") continue;
        throw err;
      }
      return this.get();
    }
  }
  /**
   * Live events of the named agent computers, or (no argument) all of them, and the Fleet's own (`fleet.changed`,
   * `agent_computer.phase`). Every stream of this Fleet shares one connection, which reconnects without losing
   * events. `await stream.ready()` before sending work you want to see from the start; `close()` (or `break`) ends it.
   * `turn` or `after` (one agent computer) replays from its journal instead.
   */
  events(agentComputers, opts = {}) {
    if (opts.turn !== void 0 || opts.after !== void 0) return this.client.events(agentComputers, opts);
    return this.client.sharedEvents(namesOf(agentComputers), opts);
  }
  /** Finished Turns of the named agent computers, or all of them, as they end. */
  turns(agentComputers, opts = {}) {
    return new TurnStream(this.client.sharedEvents(namesOf(agentComputers), opts));
  }
  /** Every agent computer and Service: phase, session, active Turn, inbox, power. */
  status() {
    return this.client.status();
  }
};
function namesOf(names) {
  return typeof names === "string" ? [names] : names;
}
function emptySpec() {
  return { apiVersion: "a12s.ai/v1alpha1", kind: "Fleet", metadata: { name: "" }, agent_computers: {} };
}
function writeHeaders(opts) {
  return opts.replace ? {} : { "if-none-match": "*" };
}
var AgentComputers = class {
  constructor(client) {
    this.client = client;
  }
  /** Adds an agent computer to the Fleet and starts it. Fails with code `exists` if the name is taken. */
  async create(name, spec = {}) {
    const { replace, ...body } = spec;
    const r = await this.client.request("PUT", `/v1/agent-computers/${enc2(name)}`, body, {
      headers: writeHeaders({ replace })
    });
    warnWrites(r);
    return r.agentComputer ? new AgentComputer(this.client, r.agentComputer) : this.get(name);
  }
  /** Throws {@link NotFound} if there is none. */
  async get(name) {
    return new AgentComputer(this.client, await this.client.request("GET", `/v1/agent-computers/${enc2(name)}`));
  }
  async list() {
    const r = await this.client.request("GET", "/v1/agent-computers");
    return (r.agentComputers ?? []).map((d) => new AgentComputer(this.client, d));
  }
};
var AgentComputer = class {
  constructor(client, data) {
    this.client = client;
    this.name = data.name;
    this.set(data);
  }
  name;
  spec;
  /** The spec with the Fleet's defaults merged in. */
  effective;
  status;
  set(d) {
    this.spec = d.spec;
    this.effective = d.effective;
    this.status = d.status;
    return this;
  }
  get path() {
    return `/v1/agent-computers/${enc2(this.name)}`;
  }
  async reload() {
    return this.set(await this.client.request("GET", this.path));
  }
  /** Changes the spec: a key set to null goes back to the Fleet's default. A busy one picks it up when idle. */
  async update(changes) {
    const r = await this.client.request("PATCH", this.path, changes);
    warnWrites(r);
    return r.agentComputer ? this.set(r.agentComputer) : this.reload();
  }
  /** Removes it from the Fleet and stops it; its drives are kept. */
  async remove() {
    await this.client.request("DELETE", this.path);
  }
  /** Sends a Message, waits for the Turn that delivers it and resolves when that Turn ends. */
  prompt(text, opts = {}) {
    return this.client.prompt(this.name, text, opts);
  }
  /** Puts a Message in the inbox and returns at once; it is delivered at the next idle moment. */
  send(text, opts = {}) {
    return this.client.send(this.name, { ...opts, body: text });
  }
  /** Its events, live, from the Fleet's shared connection; `turn` replays one Turn from its start instead. */
  events(opts = {}) {
    if (opts.turn !== void 0 || opts.after !== void 0) return this.client.events(this.name, opts);
    return this.client.sharedEvents([this.name], opts);
  }
  turns(opts = {}) {
    const { limit, ...rest } = opts;
    if (limit !== void 0) return this.pastTurns(limit);
    return new TurnStream(this.client.sharedEvents([this.name], rest));
  }
  async pastTurns(limit) {
    const r = await this.client.request("GET", `${this.path}/turns?limit=${limit}`);
    return (r.turns ?? []).map((t) => ({
      ...t,
      computer: t.agentComputer,
      tools: t.tools.map((tc) => ({ ...tc, isError: tc.isError ?? false }))
    }));
  }
  /** Stops its processes and keeps its disks, session and inbox until wake(); `allowWake` lets wakeOnMessage wake it. */
  sleep(opts = {}) {
    return this.client.sleep(this.name, opts);
  }
  /** Wakes it; `clearInbox` discards the pending Messages first. */
  wake(opts = {}) {
    return this.client.wake(this.name, opts);
  }
  /** Holds its inbox: Messages still queue, none is delivered until resume(). */
  hold(opts = {}) {
    return this.client.hold(this.name, opts);
  }
  /** Releases a hold on its inbox. */
  resume() {
    return this.client.resume(this.name);
  }
  /** Interrupts a Turn (default: the active one). */
  stop(turn) {
    return this.client.stop(this.name, turn);
  }
  inbox(opts = {}) {
    return this.client.inbox(this.name, opts);
  }
  message(id) {
    return this.client.message(this.name, id);
  }
  /** Withdraws a pending Message; a 409 A12sError names its state if it is too late. */
  withdraw(id) {
    return this.client.withdraw(this.name, id);
  }
};
var Services = class {
  constructor(client) {
    this.client = client;
  }
  /** Adds a Service to the Fleet. Fails with code `exists` if the name is taken. */
  async create(name, spec = {}) {
    const { replace, ...body } = spec;
    const r = await this.client.request("PUT", `/v1/services/${enc2(name)}`, body, {
      headers: writeHeaders({ replace })
    });
    warnWrites(r);
    return r.service ? new Service(this.client, r.service) : this.get(name);
  }
  /** Throws {@link NotFound} if there is none. */
  async get(name) {
    return new Service(this.client, await this.client.service(name));
  }
  async list() {
    return (await this.client.services()).map((s) => new Service(this.client, s));
  }
};
var Service = class {
  constructor(client, status) {
    this.client = client;
    this.name = status.name;
    this.status = status;
  }
  name;
  status;
  /** As written in the Fleet. */
  get spec() {
    return this.status.spec ?? {};
  }
  get path() {
    return `/v1/services/${enc2(this.name)}`;
  }
  async reload() {
    this.status = await this.client.service(this.name);
    return this;
  }
  /** Changes the spec as a JSON merge patch: a key set to null is removed. */
  async update(changes) {
    const r = await this.client.request("PATCH", this.path, changes);
    warnWrites(r);
    if (!r.service) return this.reload();
    this.status = r.service;
    return this;
  }
  /** Removes it from the Fleet and stops it; its state drive is kept. */
  async remove() {
    await this.client.request("DELETE", this.path);
  }
  sleep() {
    return this.power("asleep");
  }
  wake() {
    return this.power("awake");
  }
  async power(state) {
    this.status = await this.client.request("POST", `${this.path}/power`, { state });
    return this;
  }
};
var Tokens = class {
  constructor(client) {
    this.client = client;
  }
  /**
   * A token for another program; owner only. The secret is in the result once. With `prefix` it may do everything
   * the owner can for names that start with it, and nothing outside; otherwise `actions` on `agentComputers`.
   */
  create(name, opts) {
    if ("prefix" in opts) {
      return this.client.createToken({ name, prefix: opts.prefix });
    }
    return this.client.createToken({ name, actions: opts.actions, computers: opts.agentComputers ?? ["*"] });
  }
  async list() {
    return (await this.client.request("GET", "/v1/tokens")).tokens ?? [];
  }
  async remove(name) {
    await this.client.request("DELETE", `/v1/tokens/${enc2(name)}`);
  }
};
var Credentials = class {
  constructor(client) {
    this.client = client;
  }
  /** Stores the value on the Controller; agent computers that use it get the new value at once. */
  set(name, value) {
    return this.client.request("PUT", `/v1/credentials/${enc2(name)}`, { value });
  }
  async list() {
    return (await this.client.request("GET", "/v1/credentials")).credentials ?? [];
  }
  /** Fails with A12sError code `in_use` while an agent computer or Service uses it. */
  async remove(name) {
    await this.client.request("DELETE", `/v1/credentials/${enc2(name)}`);
  }
};
function warnWrites(r) {
  for (const w of r?.warnings ?? []) console.warn(`a12s: ${w}`);
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
  AgentComputer,
  AgentComputers,
  Credentials,
  EventStream,
  Fleet,
  NotFound,
  Service,
  Services,
  Tokens,
  TurnStream,
  connect
};
