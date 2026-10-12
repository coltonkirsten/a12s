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

export interface Harness {
  kind: string;
  version: string;
  model?: string;
}

/** A parsed fleet.yaml. a12s validates it; the client passes it through. */
export type Fleet = Record<string, unknown> & { metadata: { name: string } };

export interface Lock {
  a12s?: string;
  images: Record<string, string>;
  bundles: Record<string, string>;
  presets: Record<string, string>;
  services?: Record<string, string>;
}

export interface PutFleetRequest {
  fleet: Fleet;
  lock?: Lock;
  /** Credential values by name, base64-encoded (Go []byte). Kept in the Controller's memory only. */
  credentials?: Record<string, string>;
  asleep?: boolean;
  refresh?: string[];
  /** Home presets as base64 tar streams, keyed by their path in the Fleet. */
  presets?: Record<string, string>;
}

export interface ServiceChanges {
  created: string[] | null;
  updated: string[] | null;
  removed: string[] | null;
}

export interface PutFleetResponse {
  created: string[] | null;
  updated: string[] | null;
  staged: string[] | null;
  removed: string[] | null;
  lock: Lock | null;
  errors?: Record<string, string>;
  warnings?: string[];
  services: ServiceChanges;
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
  resumePending?: boolean;
  session?: string;
  activeTurn?: string;
  harness: Harness;
  image: string;
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
}

export interface StatusResponse {
  fleet: string;
  egressEnforcement: string;
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

/** The fields every journal event has. `seq` is the Computer's journal position and the SSE id. */
export interface EventEnvelope {
  v: number;
  /** The Computer that journaled it. */
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
  error?: string;
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
/** Made by the SDK, never journaled: the stream could not resume from its cursor and events were lost. */
export type GapEvent = Ev<"gap", GapData>;

/** One event of a Computer's journal. Narrow on `type` to get typed `data`. */
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
  | GapEvent;

export type EventType = A12sEvent["type"];

/** A finished Turn: what `turns()` yields and `prompt()` returns. */
export interface Turn {
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
  usage: unknown;
  startedAt?: string;
  finishedAt: string;
}

export interface ApiErrorBody {
  code: string;
  message: string;
  fix?: string;
  details?: Record<string, string>;
}

export interface TokenRequest {
  name: string;
  actions: Array<"send" | "inbox" | "status" | "events" | "power" | "stop">;
  computers: string[];
}

export interface Token {
  name: string;
  actions: string[];
  computers: string[];
  createdAt: string;
  secret?: string;
}
