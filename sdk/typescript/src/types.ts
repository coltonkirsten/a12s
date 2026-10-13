// JSON types of the a12s Controller API. They mirror internal/api in the a12s repo; the JSON is the stable contract.

export type Phase =
  | "Provisioning"
  | "Starting"
  | "Ready"
  | "Busy"
  | "Stopping"
  | "Asleep"
  | "Error"
  | "Unhealthy";

export interface Failure {
  reason: string;
  retryable: boolean;
}

/** Credentials an agent computer or Service waits for, in Provisioning; it starts by itself once they are set. */
export interface Waiting {
  credentials: string[];
  /** The commands that set them, e.g. "a12s creds set dbname". */
  fix: string;
}

export interface Harness {
  kind: string;
  version: string;
  model?: string;
}

export interface AgentComputerSpec {
  harness?: { kind: "claude-code" | "codex" | (string & {}); version?: string; model?: string };
  image?: string;
  instructions?: string;
  /** File contents by path, written into the agent computer's home. */
  files?: Record<string, string>;
  resources?: { cpu?: string | number; memory?: string | number };
  home?: { size?: string | number; preset?: string };
  autonomy?: "full" | "restricted";
  /** Credential names; their values come from `a12s up` or the credentials API. */
  credentials?: string[];
  drives?: Array<{ name: string; at: string; readOnly?: boolean }>;
  egress?: { allow?: string[] };
  /** `seccomp` is the profile JSON itself. */
  security?: { seccomp?: Record<string, unknown> };
  inbox?: { maxMessages?: number; maxBytes?: string; intro?: string };
  power?: { sleepAfterIdle?: string; wakeOnMessage?: { from: string[]; maxWakesPerHour?: number } };
  uses?: Array<string | { name: string; mcp?: boolean }>;
  grants?: { send?: string[] };
}

/** A JSON merge patch of T: a key set to null is deleted. */
export type MergePatch<T> = { [K in keyof T]?: T[K] | null };

/** A Fleet as written: the defaults and each agent computer before the defaults are merged in. */
export interface FleetSpec {
  apiVersion: string;
  kind: string;
  metadata: { name: string };
  egress?: { enforcement?: string; allowPrivate?: string[] };
  defaults?: AgentComputerSpec;
  drives?: Record<string, { size?: string | number; access: string }>;
  agent_computers: Record<string, AgentComputerSpec>;
  services?: Record<string, Record<string, unknown>>;
}

export interface Change {
  op: "create" | "update" | "remove" | (string & {});
  kind: "agentComputer" | "service" | (string & {});
  name: string;
  /** "idle" for an update that waits for a busy agent computer's next idle boundary. */
  when: "now" | "idle" | (string & {});
  /** The top-level spec fields that differ, for an update. */
  fields?: string[];
  /** Every leaf that differs, for an update, with its value before and after (absent where the field is). */
  diff?: FieldDiff[];
}

/** One leaf of a spec that differs: `path` is dotted field names ("grants.send"); lists and values are leaves. */
export interface FieldDiff {
  path: string;
  before?: unknown;
  after?: unknown;
}

export interface FleetResponse {
  version: number;
  fleet: FleetSpec | null;
}

export interface AgentComputerData {
  name: string;
  /** As written. */
  spec: AgentComputerSpec;
  /** With the Fleet's defaults merged in. */
  effective: AgentComputerSpec;
  status: ComputerStatus;
}

export interface AgentComputersResponse {
  agentComputers: AgentComputerData[];
}

export interface PutFleetRequest {
  fleet: FleetSpec;
  /** Credential values by name, base64-encoded (Go []byte). Kept in the Controller's memory only. */
  credentials?: Record<string, string>;
  asleep?: boolean;
}

export interface ServiceChanges {
  created: string[] | null;
  updated: string[] | null;
  removed: string[] | null;
}

export interface PutFleetResponse {
  /** The Fleet's version after the write; unchanged by a dry run or a write that changed nothing. */
  version: number;
  dryRun?: boolean;
  changes: Change[];
  created: string[] | null;
  updated: string[] | null;
  staged: string[] | null;
  removed: string[] | null;
  errors?: Record<string, string>;
  warnings?: string[];
  services: ServiceChanges;
  /** The written agent computer, after a PUT or PATCH of one. */
  agentComputer?: AgentComputerData;
  /** The written Service, after a PUT or PATCH of one. */
  service?: ServiceStatus;
}

export interface LimitValue {
  value: number;
  source: string;
}

export interface InboxSummary {
  computer: string;
  held: boolean;
  holdReason?: string;
  diagnostic?: string;
  pending: number;
  claimed: number;
  limits: { maxMessages: LimitValue; maxBytes: LimitValue };
  usage: { messages: number; bytes: number };
  overLimit: boolean;
  ended24h: { expired: number; replaced: number; withdrawn: number; discarded: number };
  orphaned?: boolean;
  sessionOverride?: string;
}

export interface PowerStatus {
  sleepAfterIdle?: string;
  wakeOnMessage?: string[];
  maxWakesPerHour?: number;
  autoWake: string;
  reason?: string;
  fix?: string;
  wakesLastHour: number;
  nextWakeAt?: string;
  idleSince?: string;
  sleptBy?: string;
  sleptAt?: string;
}

export interface ComputerStatus {
  name: string;
  phase: Phase;
  error?: Failure;
  waiting?: Waiting;
  resumePending?: boolean;
  session?: string;
  activeTurn?: string;
  harness: Harness;
  image: string;
  /** The Fleet's security.seccomp path, when the Computer runs with a custom seccomp profile. */
  seccomp?: string;
  autonomy: string;
  generation: number;
  stagedGeneration?: number;
  credentialGeneration?: number;
  /** A quantity such as "10Gi". */
  homeSize?: string;
  drives?: string[];
  egressMode?: string;
  warnings?: string[];
  inbox?: InboxSummary;
  /** Set only on a wake --clear-inbox answer: how many Messages were discarded. */
  inboxCleared?: number;
  power?: PowerStatus;
}

export interface DriveRow {
  name: string;
  kind: "home" | "work" | "state";
  computer?: string;
  service?: string;
  /** A quantity such as "10Gi". */
  size?: string;
  access?: string;
  attachedTo?: string[];
  orphaned?: boolean;
}

export interface ServiceStatus {
  name: string;
  phase: Phase;
  error?: Failure;
  waiting?: Waiting;
  image: string;
  pin?: string;
  generation: number;
  restarts: number;
  health?: string;
  healthSince?: string;
  send?: string[];
  uses?: string[];
  usedBy?: string[];
  publish?: string[];
  ports?: string[];
  stateDrive?: string;
  egressMode?: string;
  warnings?: string[];
  /** The Service as written in the Fleet. */
  spec?: Record<string, unknown>;
}

export interface StatusResponse {
  fleet: string;
  egressEnforcement: string;
  agentComputers: ComputerStatus[];
  /** @deprecated `agentComputers` under its old name. */
  computers: ComputerStatus[];
  orphanedDrives?: DriveRow[];
  credentialsLoaded: boolean;
  warnings?: string[];
  orphanedInboxes?: InboxSummary[];
  services?: ServiceStatus[];
}

export type MessageState =
  | "pending"
  | "claimed"
  | "delivered"
  | "discarded"
  | "withdrawn"
  | "expired"
  | "replaced";

export interface EnqueueRequest {
  body: string;
  /** Free-form sender label; the authenticated principal is recorded separately as `via`. */
  sender?: string;
  metadata?: Record<string, string>;
  fresh?: boolean;
  /** A retry with the same key returns the original Message. The client sends a random one if unset. */
  idempotencyKey?: string;
  /** Seconds; 0 means now or never. */
  expires?: number;
  replaceKey?: string;
}

export interface EnqueueResponse {
  id: string;
  seq: number;
  createdAt: string;
  expiresAt: string | null;
  state: MessageState;
  position: number;
  phase?: Phase;
  replaced?: string;
  batch?: string;
  turn?: string;
  outcome?: string;
}

export interface Message {
  id: string;
  computer: string;
  seq: number;
  sender: string;
  via: string;
  createdAt: string;
  body?: string;
  metadata?: Record<string, string>;
  fresh: boolean;
  expiresAt: string | null;
  replaceKey?: string;
  superseded: number;
  replacedBy?: string;
  state: MessageState;
  batch?: string;
  turn?: string;
  attempts: number;
  bytes: number;
  endedAt?: string;
}

export interface InboxListResponse extends InboxSummary {
  messages: Message[];
}

export interface InboxListOptions {
  /** A Message state, or "all". Default: pending and claimed. */
  state?: MessageState | "all";
  bodies?: boolean;
  after?: number;
}

/** The fields every event has. `seq` is the agent computer's journal position (0 for the Controller's own events). */
export interface EventEnvelope {
  v: number;
  /** The agent computer it is about; absent on `fleet.changed` and in journals older than the field. */
  agentComputer?: string;
  /** `agentComputer` under its old name, for one release. */
  computer: string;
  session: string;
  /** The Turn it belongs to; "" for events outside a Turn. */
  turn: string;
  seq: number;
  ts: string;
  type: string;
  data?: unknown;
}

type Ev<T extends string, D> = Omit<EventEnvelope, "type" | "data"> & { type: T; data: D };

/** One Message a Turn carries, as listed in its `turn.started` event. */
export interface TurnInboxMessage {
  id: string;
  seq: number;
  /** The sender's own label (unverified). */
  sender: string;
  /** Who really sent it: `local`, `token:<name>`, `service:<name>` or `computer:<name>`. */
  via: string;
  createdAt: string;
  bytes: number;
  key?: string;
  superseded?: number;
}

export interface TurnInbox {
  batch: string;
  pendingAfter: number;
  messages: TurnInboxMessage[];
}

export interface TurnStartedData {
  harness: Harness;
  model?: string;
  resumed: boolean;
  /** The prompt as the harness received it (an inbox Batch is rendered with its frame). */
  prompt: string;
  cwd: string;
  requestId?: string;
  /** The Messages this Turn delivers. Absent for a continuation after `restart_harness`. */
  inbox?: TurnInbox;
  continuation?: string;
}

export interface TurnFinishedData {
  status: "completed" | "failed" | "interrupted";
  /** The harness's own usage object (tokens, cost), or null. */
  usage: unknown;
  /** The same usage, normalized across harnesses, when the harness reported it. */
  tokens?: TokenUsage;
  error?: string;
}

/** A Turn's token usage, the same for every harness. `input` excludes cached input. */
export interface TokenUsage {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

/** The egress proxy refused a connection. */
export interface EgressDeniedData {
  host: string;
  port: number;
  reason?: string;
}

/** The Fleet's version moved; `names` are the agent computers and Services the write changed. */
export interface FleetChangedData {
  version: number;
  /** Who wrote it. */
  by: string;
  names: string[];
}

export interface PhaseData {
  phase: Phase;
  error?: Failure;
  waiting?: Waiting;
}

/** A Service's phase, health or restart count changed. */
export interface ServicePhaseData {
  service: string;
  phase: Phase;
  /** "ok", or why the last health check failed. */
  health?: string;
  restarts: number;
  error?: Failure;
  waiting?: Waiting;
}

export interface ToolStartedData {
  id: string;
  /** MCP tools are named `mcp__<server>__<tool>`, e.g. `mcp__a12s__send`. */
  name: string;
  input?: unknown;
}

export interface ToolFinishedData {
  id: string;
  name: string;
  isError: boolean;
  output?: unknown;
}

export interface DiagnosticData {
  message: string;
  data?: unknown;
  source?: string;
}

export interface ResumeFailedData {
  sessionId: string;
  reason: string;
  at: string;
}

/** The data of a `gap` event: the events after `after` were lost. */
export interface GapData {
  /** The cursor the stream asked to resume after, if any. */
  after?: number;
  /** The Controller's last journal seq when it answered, if it said. */
  head?: number;
  lastEventId?: string;
}

export type TurnStartedEvent = Ev<"turn.started", TurnStartedData>;
export type TurnDispatchedEvent = Ev<"turn.dispatched", Record<string, never>>;
export type TextDeltaEvent = Ev<"text.delta", { text: string }>;
export type ToolStartedEvent = Ev<"tool.started", ToolStartedData>;
export type ToolFinishedEvent = Ev<"tool.finished", ToolFinishedData>;
export type PermissionEvent = Ev<"permission", { decision: "allow" | "deny"; kind: string; tool?: string; detail?: unknown }>;
export type DiagnosticEvent = Ev<"diagnostic", DiagnosticData>;
export type TurnFinishedEvent = Ev<"turn.finished", TurnFinishedData>;
export type SessionChangedEvent = Ev<"session.changed", { from: string | null; to: string }>;
export type ResumeFailedEvent = Ev<"resume.failed", ResumeFailedData>;
export type LifecycleEvent = Ev<"lifecycle", { phase: string; [k: string]: unknown }>;
export type HarnessRestartedEvent = Ev<"harness.restarted", Record<string, unknown>>;
export type ContinuationAbandonedEvent = Ev<"continuation.abandoned", { continuation: string; reason: string; cancelled?: boolean }>;
export type TurnRefusedEvent = Ev<"turn.refused", { requestId: string }>;
/** A Message that ended without delivery (cleared, withdrawn, expired, replaced), body included. */
export type InboxDiscardedEvent = Ev<"inbox.discarded", Record<string, unknown>>;
export type EgressDeniedEvent = Ev<"egress.denied", EgressDeniedData>;
/** Made by the Controller, on the fleet-wide stream only. */
export type FleetChangedEvent = Ev<"fleet.changed", FleetChangedData>;
/** Made by the Controller, on the fleet-wide stream only. */
export type PhaseEvent = Ev<"agent_computer.phase", PhaseData>;
/** Made by the Controller, on the fleet-wide stream only, after any change to the agent computer's inbox (a burst is one event). */
export type InboxChangedEvent = Ev<"inbox.changed", InboxSummary>;
/** Made by the Controller, on the fleet-wide stream only; `agentComputer` is empty, the Service is in `data`. */
export type ServicePhaseEvent = Ev<"service.phase", ServicePhaseData>;
/** Made by the SDK, never journaled: the stream could not resume from its cursor and events were lost. */
export type GapEvent = Ev<"gap", GapData>;

/** One event of an agent computer's journal, or the Controller's own on the fleet-wide stream. Narrow on `type` to get typed `data`. */
export type A12sEvent =
  | TurnStartedEvent
  | TurnDispatchedEvent
  | TextDeltaEvent
  | ToolStartedEvent
  | ToolFinishedEvent
  | PermissionEvent
  | DiagnosticEvent
  | TurnFinishedEvent
  | SessionChangedEvent
  | ResumeFailedEvent
  | LifecycleEvent
  | HarnessRestartedEvent
  | ContinuationAbandonedEvent
  | TurnRefusedEvent
  | InboxDiscardedEvent
  | EgressDeniedEvent
  | FleetChangedEvent
  | PhaseEvent
  | InboxChangedEvent
  | ServicePhaseEvent
  | GapEvent;

export type EventType = A12sEvent["type"];

/** One tool call in a Turn. A `send` to another Computer is `mcp__a12s__send` with `input.body`. */
export interface ToolCall {
  id: string;
  name: string;
  input?: unknown;
  output?: unknown;
  isError: boolean;
}

/** A finished Turn: what `turns()` yields and `wait()` returns. */
export interface Turn {
  agentComputer: string;
  /** `agentComputer` under its old name, for one release. */
  computer: string;
  /** The Turn id. */
  id: string;
  session: string;
  status: TurnFinishedData["status"];
  /** Everything the agent said in the Turn, concatenated. */
  text: string;
  error?: string;
  /** The Messages it delivered (who sent what): empty for a continuation, or if the stream joined mid-Turn. */
  messages: TurnInboxMessage[];
  /** The tools it called, in order, with their input and output: what it `send`s to other Computers is here. */
  tools: ToolCall[];
  usage: unknown;
  tokens?: TokenUsage;
  startedAt?: string;
  finishedAt: string;
}

/** Past Turns, newest first, as GET /v1/agent-computers/{name}/turns sends them. */
export interface TurnsResponse {
  turns: Array<Omit<Turn, "computer" | "tools"> & { tools: Array<Omit<ToolCall, "isError"> & { isError?: boolean }> }>;
}

export interface ApiErrorBody {
  code: string;
  message: string;
  fix?: string;
  details?: Record<string, string>;
}

export type TokenAction = "send" | "inbox" | "status" | "events" | "power" | "stop";

/** Either `prefix`, or `actions` on `computers` (agent computer names, or "*"). */
export interface TokenRequest {
  name: string;
  actions?: TokenAction[];
  computers?: string[];
  /** Every power over agent computers, Services and credentials whose names start with it; none outside. */
  prefix?: string;
}

/** A credential's name, whether it has a value, and who uses it. */
export interface CredentialInfo {
  name: string;
  set: boolean;
  usedBy?: string[];
  /** When the value last changed (RFC 3339); absent if unknown. */
  updatedAt?: string;
}

export interface CredentialsResponse {
  credentials: CredentialInfo[];
}

export interface Token {
  name: string;
  actions: string[];
  computers: string[];
  prefix?: string;
  createdAt: string;
  /** When it last authenticated (RFC 3339, recorded at most once a minute); absent if never. */
  lastUsedAt?: string;
  secret?: string;
}
