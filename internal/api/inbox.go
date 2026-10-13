package api

import "time"

// A Principal is "local", "token:<name>", "service:<name>" or
// "computer:<name>". It comes from the transport, never from a body.
const (
	PrincipalLocal = "local"
)

const (
	MsgPending   = "pending"
	MsgClaimed   = "claimed"
	MsgDelivered = "delivered"
	MsgDiscarded = "discarded"
	MsgWithdrawn = "withdrawn"
	MsgExpired   = "expired"
	MsgReplaced  = "replaced"
)

type EnqueueRequest struct {
	Body     string            `json:"body"`
	Sender   string            `json:"sender,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Fresh    bool              `json:"fresh,omitempty"`
	// IdempotencyKey is scoped to (principal, inbox).
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// Expires is in whole seconds: absent waits indefinitely, 0 is now or
	// never, 1..2592000 is a window.
	Expires *int64 `json:"expires,omitempty"`
	// ReplaceKey supersedes the pending Message with the same key from the
	// same principal only.
	ReplaceKey string `json:"replaceKey,omitempty"`
}

// EnqueueResponse is a 201, or 200 for an idempotent repeat.
type EnqueueResponse struct {
	ID        string     `json:"id"`
	Seq       int64      `json:"seq"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
	State     string     `json:"state"`
	Position  int        `json:"position"`
	Phase     Phase      `json:"phase,omitempty"`
	Replaced  string     `json:"replaced,omitempty"`
	// Batch and Turn are set only for expires: 0; Outcome is "unknown" on a 202.
	Batch   string `json:"batch,omitempty"`
	Turn    string `json:"turn,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

type Message struct {
	ID         string            `json:"id"`
	Computer   string            `json:"computer"`
	Seq        int64             `json:"seq"`
	Sender     string            `json:"sender"`
	Via        string            `json:"via"`
	CreatedAt  time.Time         `json:"createdAt"`
	Body       *string           `json:"body,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Fresh      bool              `json:"fresh"`
	ExpiresAt  *time.Time        `json:"expiresAt"`
	ReplaceKey string            `json:"replaceKey,omitempty"`
	Superseded int64             `json:"superseded"`
	ReplacedBy string            `json:"replacedBy,omitempty"`
	State      string            `json:"state"`
	Batch      string            `json:"batch,omitempty"`
	Turn       string            `json:"turn,omitempty"`
	Attempts   int               `json:"attempts"`
	// Bytes is the rendered size reserved, not the body length.
	Bytes   int64      `json:"bytes"`
	EndedAt *time.Time `json:"endedAt,omitempty"`
}

// LimitValue source is "computer", "defaults" or "builtin".
type LimitValue struct {
	Value  int64  `json:"value"`
	Source string `json:"source"`
}

type InboxLimits struct {
	MaxMessages LimitValue `json:"maxMessages"`
	MaxBytes    LimitValue `json:"maxBytes"`
}

// InboxUsage counts pending + claimed.
type InboxUsage struct {
	Messages int64 `json:"messages"`
	Bytes    int64 `json:"bytes"`
}

// InboxEnded counts only the last 24h.
type InboxEnded struct {
	Expired   int64 `json:"expired"`
	Replaced  int64 `json:"replaced"`
	Withdrawn int64 `json:"withdrawn"`
	Discarded int64 `json:"discarded"`
}

type InboxSummary struct {
	Computer   string      `json:"computer"`
	Held       bool        `json:"held"`
	HoldReason string      `json:"holdReason,omitempty"`
	Diagnostic string      `json:"diagnostic,omitempty"`
	Pending    int64       `json:"pending"`
	Claimed    int64       `json:"claimed"`
	Limits     InboxLimits `json:"limits"`
	Usage      InboxUsage  `json:"usage"`
	OverLimit  bool        `json:"overLimit"`
	Ended24h   InboxEnded  `json:"ended24h"`
	Orphaned   bool        `json:"orphaned,omitempty"`
	// SessionOverride is "new" while `inbox resume --new` waits for the next Batch.
	SessionOverride string `json:"sessionOverride,omitempty"`
}

type InboxListResponse struct {
	InboxSummary
	Messages []Message `json:"messages"`
}

type ClearRequest struct {
	// ThroughSeq nil means the current head.
	ThroughSeq *int64 `json:"throughSeq,omitempty"`
}

type ClearResponse struct {
	Discarded int `json:"discarded"`
}

type HoldRequest struct {
	Reason string `json:"reason,omitempty"`
}

type ResumeRequest struct {
	Session string `json:"session,omitempty"` // SessionCurrent (default) or SessionNew
}

const (
	HoldOperator      = "operator"
	HoldUndeliverable = "undeliverable"
	HoldResumeFailed  = "resume_failed"
	HoldOversized     = "oversized"
	HoldFailed        = "failed"
)

const (
	ActionSend   = "send"   // enqueue; get and withdraw own Messages
	ActionInbox  = "inbox"  // list, read bodies, clear, hold, resume
	ActionStatus = "status" // GET /v1/status
	ActionEvents = "events" // read a Computer's journal stream
	ActionPower  = "power"  // sleep and wake
	ActionStop   = "stop"   // interrupt a Turn
)

var TokenActions = []string{ActionSend, ActionInbox, ActionStatus, ActionEvents, ActionPower, ActionStop}

// TokenRequest is owner only. A token has either a Prefix, or Actions on Computers.
type TokenRequest struct {
	Name      string   `json:"name"`
	Actions   []string `json:"actions,omitempty"`
	Computers []string `json:"computers,omitempty"` // ["*"] for every agent computer
	// Prefix makes a token that may do everything the owner can for agent computers, Services and credentials whose
	// names start with it, and nothing else.
	Prefix string `json:"prefix,omitempty"`
}

// Token.Secret is only returned once, on create.
type Token struct {
	Name      string    `json:"name"`
	Actions   []string  `json:"actions"`
	Computers []string  `json:"computers"`
	Prefix    string    `json:"prefix,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	// LastUsedAt is recorded at most once a minute.
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Secret     string     `json:"secret,omitempty"`
}

type TokensResponse struct {
	Tokens []Token `json:"tokens"`
}

// Power policy autoWake values.
const (
	AutoWakeOn          = "on"
	AutoWakeOff         = "off"
	AutoWakeSuppressed  = "suppressed"   // slept with hold: until a manual wake
	AutoWakePaused      = "paused"       // credentials not loaded, or the last wake failed
	AutoWakeRateLimited = "rate_limited" // maxWakesPerHour reached
)

const (
	SleptAuto   = "auto"
	SleptManual = "manual"
	SleptHeld   = "held"
)

type PowerStatus struct {
	SleepAfterIdle  string     `json:"sleepAfterIdle,omitempty"` // "off" or a duration
	WakeOnMessage   []string   `json:"wakeOnMessage,omitempty"`
	MaxWakesPerHour int        `json:"maxWakesPerHour,omitempty"`
	AutoWake        string     `json:"autoWake"`
	Reason          string     `json:"reason,omitempty"`
	Fix             string     `json:"fix,omitempty"`
	WakesLastHour   int        `json:"wakesLastHour"`
	NextWakeAt      *time.Time `json:"nextWakeAt,omitempty"`
	IdleSince       *time.Time `json:"idleSince,omitempty"`
	SleptBy         string     `json:"sleptBy,omitempty"`
	SleptAt         *time.Time `json:"sleptAt,omitempty"`
}
