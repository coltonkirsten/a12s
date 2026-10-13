// Runs in Node, Deno, Bun and browsers: no node: imports here (`npm run check:browser` enforces it).
import {
  A12sClient,
  A12sError,
  type A12sClientOptions,
  type EventsOptions,
  TurnStream,
  type EventStream,
  type PromptOptions,
  type PromptResult,
  type SharedEventsOptions,
} from "./client.js";
import type {
  AgentComputerData,
  AgentComputersResponse,
  AgentComputerSpec,
  Change,
  ComputerStatus,
  CredentialInfo,
  CredentialsResponse,
  EnqueueRequest,
  EnqueueResponse,
  FleetResponse,
  FleetSpec,
  InboxListOptions,
  InboxListResponse,
  MergePatch,
  Message,
  PutFleetResponse,
  ServiceStatus,
  StatusResponse,
  Token,
  TokenAction,
  Turn,
  TurnsResponse,
} from "./types.js";

const enc = encodeURIComponent;
const maxReplaceTries = 5;

/** The Fleet as written, and its version for a conditional write. */
export interface FleetState {
  version: number;
  spec: FleetSpec;
  /** What changed through the API since the last whole-Fleet write. */
  drift: Change[];
}

export interface CreateOptions {
  /** Overwrite an existing one instead of failing with A12sError code `exists`. */
  replace?: boolean;
}

/**
 * A running Fleet. Its agent computers, Services and tokens are in {@link agentComputers}, {@link services} and
 * {@link tokens}.
 *
 * ```ts
 * const fleet = await Fleet.connect();
 * const hello = await fleet.agentComputers.create("hello", { instructions: "Be helpful" });
 * await hello.prompt("hi");
 * await hello.remove();
 * ```
 */
export class Fleet {
  /** The lower-level client the handles use, for API calls that have no method here. */
  readonly client: A12sClient;
  readonly agentComputers: AgentComputers;
  readonly services: Services;
  readonly tokens: Tokens;
  readonly credentials: Credentials;

  /** No discovery: `url` and `token` as given, by default the page's own origin. */
  constructor(opts: A12sClientOptions = {}) {
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
  static async connect(opts: A12sClientOptions = {}): Promise<Fleet> {
    return new Fleet(opts);
  }

  get url(): string {
    return this.client.url;
  }

  async get(): Promise<FleetState> {
    const r = await this.client.request<FleetResponse>("GET", "/v1/fleet");
    return { version: r.version, spec: r.fleet ?? emptySpec(), drift: r.drift ?? [] };
  }

  /**
   * Reads the Fleet, lets `fn` change it (in place, or by returning a new spec) and writes it back only if nobody
   * wrote in between; otherwise it reads again and retries, up to 5 times. `spec.defaults` is always present.
   */
  async replace(fn: (spec: FleetSpec) => FleetSpec | void | Promise<FleetSpec | void>): Promise<FleetState> {
    for (let attempt = 1; ; attempt++) {
      const cur = await this.get();
      cur.spec.defaults ??= {};
      const next = (await fn(cur.spec)) ?? cur.spec;
      try {
        warnWrites(
          await this.client.request<PutFleetResponse>("PUT", "/v1/fleet", { fleet: next }, {
            headers: { "if-match": `"${cur.version}"` },
          }),
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
  events(agentComputers?: string | string[], opts: EventsOptions = {}): EventStream {
    if (opts.turn !== undefined || opts.after !== undefined) return this.client.events(agentComputers, opts);
    return this.client.sharedEvents(namesOf(agentComputers), opts);
  }

  /** Finished Turns of the named agent computers, or all of them, as they end. */
  turns(agentComputers?: string | string[], opts: SharedEventsOptions = {}): TurnStream {
    return new TurnStream(this.client.sharedEvents(namesOf(agentComputers), opts));
  }

  /** Every agent computer and Service: phase, session, active Turn, inbox, power. */
  status(): Promise<StatusResponse> {
    return this.client.status();
  }
}

function namesOf(names?: string | string[]): string[] | undefined {
  return typeof names === "string" ? [names] : names;
}

function emptySpec(): FleetSpec {
  return { apiVersion: "a12s.ai/v1alpha1", kind: "Fleet", metadata: { name: "" }, agent_computers: {} };
}

function writeHeaders(opts: CreateOptions): Record<string, string> {
  return opts.replace ? {} : { "if-none-match": "*" };
}

export class AgentComputers {
  constructor(private readonly client: A12sClient) {}

  /** Adds an agent computer to the Fleet and starts it. Fails with code `exists` if the name is taken. */
  async create(name: string, spec: AgentComputerSpec & CreateOptions = {}): Promise<AgentComputer> {
    const { replace, ...body } = spec;
    const r = await this.client.request<PutFleetResponse>("PUT", `/v1/agent-computers/${enc(name)}`, body, {
      headers: writeHeaders({ replace }),
    });
    warnWrites(r);
    return r.agentComputer ? new AgentComputer(this.client, r.agentComputer) : this.get(name);
  }

  /** Throws {@link NotFound} if there is none. */
  async get(name: string): Promise<AgentComputer> {
    return new AgentComputer(this.client, await this.client.request("GET", `/v1/agent-computers/${enc(name)}`));
  }

  async list(): Promise<AgentComputer[]> {
    const r = await this.client.request<AgentComputersResponse>("GET", "/v1/agent-computers");
    return (r.agentComputers ?? []).map((d) => new AgentComputer(this.client, d));
  }
}

/** A handle on one agent computer. `spec`, `effective` and `status` are as of the last create, get, reload or update. */
export class AgentComputer {
  readonly name: string;
  spec!: AgentComputerSpec;
  /** The spec with the Fleet's defaults merged in. */
  effective!: AgentComputerSpec;
  status!: ComputerStatus;

  constructor(
    private readonly client: A12sClient,
    data: AgentComputerData,
  ) {
    this.name = data.name;
    this.set(data);
  }

  private set(d: AgentComputerData): this {
    this.spec = d.spec;
    this.effective = d.effective;
    this.status = d.status;
    return this;
  }

  private get path(): string {
    return `/v1/agent-computers/${enc(this.name)}`;
  }

  async reload(): Promise<this> {
    return this.set(await this.client.request("GET", this.path));
  }

  /** Changes the spec: a key set to null goes back to the Fleet's default. A busy one picks it up when idle. */
  async update(changes: MergePatch<AgentComputerSpec>): Promise<this> {
    const r = await this.client.request<PutFleetResponse>("PATCH", this.path, changes);
    warnWrites(r);
    return r.agentComputer ? this.set(r.agentComputer) : this.reload();
  }

  /** Removes it from the Fleet and stops it; its drives are kept. */
  async remove(): Promise<void> {
    await this.client.request("DELETE", this.path);
  }

  /** Sends a Message, waits for the Turn that delivers it and resolves when that Turn ends. */
  prompt(text: string, opts: PromptOptions = {}): Promise<PromptResult> {
    return this.client.prompt(this.name, text, opts);
  }

  /** Puts a Message in the inbox and returns at once; it is delivered at the next idle moment. */
  send(text: string, opts: Omit<EnqueueRequest, "body"> = {}): Promise<EnqueueResponse> {
    return this.client.send(this.name, { ...opts, body: text });
  }

  /** Its events, live, from the Fleet's shared connection; `turn` replays one Turn from its start instead. */
  events(opts: EventsOptions = {}): EventStream {
    if (opts.turn !== undefined || opts.after !== undefined) return this.client.events(this.name, opts);
    return this.client.sharedEvents([this.name], opts);
  }

  /** Its last `limit` finished Turns, newest first. */
  turns(opts: { limit: number }): Promise<Turn[]>;
  /** Its Turns as they end, from the Fleet's shared connection. */
  turns(opts?: SharedEventsOptions & { limit?: undefined }): TurnStream;
  turns(opts: SharedEventsOptions & { limit?: number } = {}): TurnStream | Promise<Turn[]> {
    const { limit, ...rest } = opts;
    if (limit !== undefined) return this.pastTurns(limit);
    return new TurnStream(this.client.sharedEvents([this.name], rest));
  }

  private async pastTurns(limit: number): Promise<Turn[]> {
    const r = await this.client.request<TurnsResponse>("GET", `${this.path}/turns?limit=${limit}`);
    return (r.turns ?? []).map((t) => ({
      ...t,
      computer: t.agentComputer,
      tools: t.tools.map((tc) => ({ ...tc, isError: tc.isError ?? false })),
    }));
  }

  /** Stops its processes and keeps its disks, session and inbox; a sender in wakeOnMessage wakes it, unless `hold`. */
  sleep(opts: { hold?: boolean } = {}): Promise<void> {
    return this.client.sleep(this.name, opts);
  }

  /** Wakes it; `clearInbox` discards the pending Messages first. */
  wake(opts: { clearInbox?: boolean } = {}): Promise<void> {
    return this.client.wake(this.name, opts);
  }

  /** Holds its inbox: Messages still queue, none is delivered until resume(). */
  hold(opts: { reason?: string } = {}): Promise<InboxListResponse> {
    return this.client.hold(this.name, opts);
  }

  /** Releases a hold on its inbox. */
  resume(): Promise<InboxListResponse> {
    return this.client.resume(this.name);
  }

  /** Interrupts a Turn (default: the active one). */
  stop(turn?: string): Promise<void> {
    return this.client.stop(this.name, turn);
  }

  inbox(opts: InboxListOptions = {}): Promise<InboxListResponse> {
    return this.client.inbox(this.name, opts);
  }

  message(id: string): Promise<Message> {
    return this.client.message(this.name, id);
  }

  /** Withdraws a pending Message; a 409 A12sError names its state if it is too late. */
  withdraw(id: string): Promise<Message> {
    return this.client.withdraw(this.name, id);
  }
}

export class Services {
  constructor(private readonly client: A12sClient) {}

  /** Adds a Service to the Fleet. Fails with code `exists` if the name is taken. */
  async create(name: string, spec: Record<string, unknown> & CreateOptions = {}): Promise<Service> {
    const { replace, ...body } = spec;
    const r = await this.client.request<PutFleetResponse>("PUT", `/v1/services/${enc(name)}`, body, {
      headers: writeHeaders({ replace }),
    });
    warnWrites(r);
    return r.service ? new Service(this.client, r.service) : this.get(name);
  }

  /** Throws {@link NotFound} if there is none. */
  async get(name: string): Promise<Service> {
    return new Service(this.client, await this.client.service(name));
  }

  async list(): Promise<Service[]> {
    return (await this.client.services()).map((s) => new Service(this.client, s));
  }
}

/** A handle on one Service. `spec` and `status` are as of the last create, get, reload, update or power change. */
export class Service {
  readonly name: string;
  status: ServiceStatus;

  constructor(
    private readonly client: A12sClient,
    status: ServiceStatus,
  ) {
    this.name = status.name;
    this.status = status;
  }

  /** As written in the Fleet. */
  get spec(): Record<string, unknown> {
    return this.status.spec ?? {};
  }

  private get path(): string {
    return `/v1/services/${enc(this.name)}`;
  }

  async reload(): Promise<this> {
    this.status = await this.client.service(this.name);
    return this;
  }

  /** Changes the spec as a JSON merge patch: a key set to null is removed. */
  async update(changes: Record<string, unknown>): Promise<this> {
    const r = await this.client.request<PutFleetResponse>("PATCH", this.path, changes);
    warnWrites(r);
    if (!r.service) return this.reload();
    this.status = r.service;
    return this;
  }

  /** Removes it from the Fleet and stops it; its state drive is kept. */
  async remove(): Promise<void> {
    await this.client.request("DELETE", this.path);
  }

  sleep(): Promise<this> {
    return this.power("asleep");
  }

  wake(): Promise<this> {
    return this.power("awake");
  }

  private async power(state: "asleep" | "awake"): Promise<this> {
    this.status = await this.client.request("POST", `${this.path}/power`, { state });
    return this;
  }
}

export class Tokens {
  constructor(private readonly client: A12sClient) {}

  /**
   * A token for another program; owner only. The secret is in the result once. With `prefix` it may do everything
   * the owner can for names that start with it, and nothing outside; otherwise `actions` on `agentComputers`.
   */
  create(name: string, opts: { prefix: string } | { actions: TokenAction[]; agentComputers?: string[] }): Promise<Token> {
    if ("prefix" in opts) {
      return this.client.createToken({ name, prefix: opts.prefix });
    }
    return this.client.createToken({ name, actions: opts.actions, computers: opts.agentComputers ?? ["*"] });
  }

  async list(): Promise<Token[]> {
    return (await this.client.request<{ tokens: Token[] }>("GET", "/v1/tokens")).tokens ?? [];
  }

  async remove(name: string): Promise<void> {
    await this.client.request("DELETE", `/v1/tokens/${enc(name)}`);
  }
}

/** Credentials by name: set once, then name them in a spec's `credentials`. Values are never read back. */
export class Credentials {
  constructor(private readonly client: A12sClient) {}

  /** Stores the value on the Controller; agent computers that use it get the new value at once. */
  set(name: string, value: string): Promise<CredentialInfo> {
    return this.client.request<CredentialInfo>("PUT", `/v1/credentials/${enc(name)}`, { value });
  }

  async list(): Promise<CredentialInfo[]> {
    return (await this.client.request<CredentialsResponse>("GET", "/v1/credentials")).credentials ?? [];
  }

  /** Fails with A12sError code `in_use` while an agent computer or Service uses it. */
  async remove(name: string): Promise<void> {
    await this.client.request("DELETE", `/v1/credentials/${enc(name)}`);
  }
}

/** A write's warnings (a grant to an agent computer that doesn't exist yet, a missing harness credential) go to the
 * console instead of being dropped. */
function warnWrites(r: PutFleetResponse | undefined): void {
  for (const w of r?.warnings ?? []) console.warn(`a12s: ${w}`);
}
