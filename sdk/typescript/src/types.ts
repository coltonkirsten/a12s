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
  homeSize?: number;
  drives?: string[];
  egressMode?: string;
  warnings?: string[];
  inbox?: InboxSummary;
  power?: PowerStatus;
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
  credentialsLoaded: boolean;
  warnings?: string[];
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

export type EventType =
  | "turn.started"
  | "text.delta"
  | "tool.started"
  | "tool.finished"
  | "permission"
  | "diagnostic"
  | "turn.finished"
  | "session.changed"
  | "resume.failed"
  | "lifecycle"
  | "harness.restarted"
  | "turn.dispatched"
  | "continuation.abandoned"
  | "turn.refused"
  | "inbox.discarded"
  /** Client-side, from the SDK's event stream: events were lost (see GapEvent). Never in the journal. */
  | "gap"
  | (string & {});

/** One journal event of a Computer (or, on a Message stream, one state change of the Message). */
export interface A12sEvent<D = any> {
  v: number;
  computer: string;
  session: string;
  turn: string;
  seq: number;
  ts: string;
  type: EventType;
  data?: D;
}

export interface TurnFinishedData {
  status: "completed" | "failed" | "interrupted";
  usage: unknown;
  error?: string;
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
