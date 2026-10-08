package api

import "time"

// Principal kinds (enhancements §2): the authenticated identity of a caller.
// A Principal is "local", "token:<name>", "service:<name>" or
// "computer:<name>". It comes from the transport, never from a body.
const (
	PrincipalLocal = "local"
)

// Message states (enhancements §4.2).
const (
	MsgPending   = "pending"
	MsgClaimed   = "claimed"
	MsgDelivered = "delivered"
	MsgDiscarded = "discarded"
	MsgWithdrawn = "withdrawn"
	MsgExpired   = "expired"
	MsgReplaced  = "replaced"
)

// EnqueueRequest is POST /v1/computers/{c}/inbox.
type EnqueueRequest struct {
	Body     string            `json:"body"`
	Sender   string            `json:"sender,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Fresh    bool              `json:"fresh,omitempty"`
	// IdempotencyKey makes a retry return the original Message. Scoped to
	// (principal, inbox).
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// Expires is in whole seconds: absent waits indefinitely, 0 is now or
	// never, 1..2592000 is a window (§4.11).
	Expires *int64 `json:"expires,omitempty"`
	// ReplaceKey supersedes the pending Message with the same key from the
	// same principal (§4.12).
	ReplaceKey string `json:"replaceKey,omitempty"`
}

// EnqueueResponse is the 201 (or 200 for an idempotent repeat) answer.
type EnqueueResponse struct {
	ID        string     `json:"id"`
	Seq       int64      `json:"seq"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
	State     string     `json:"state"`
	// Position is how many outstanding Messages are ahead of this one.
	Position int   `json:"position"`
	Phase    Phase `json:"phase,omitempty"`
	// Replaced is the id of the pending Message this one superseded.
	Replaced string `json:"replaced,omitempty"`
	// Batch and Turn are set when the Message was claimed at once
	// (expires: 0); Outcome is "unknown" on a 202.
	Batch   string `json:"batch,omitempty"`
	Turn    string `json:"turn,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// Message is one inbox row (§4.2).
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
	// Bytes is the rendered size reserved for the Message.
	Bytes int64 `json:"bytes"`
	// EndedAt is when the Message reached a terminal state.
	EndedAt *time.Time `json:"endedAt,omitempty"`
}

// LimitValue is a resolved inbox limit and where it came from: "computer",
// "defaults" or "builtin".
type LimitValue struct {
	Value  int64  `json:"value"`
	Source string `json:"source"`
}

// InboxLimits are an inbox's resolved limits (§4.13).
type InboxLimits struct {
	MaxMessages LimitValue `json:"maxMessages"`
	MaxBytes    LimitValue `json:"maxBytes"`
}

// InboxUsage is what is outstanding (pending + claimed).
type InboxUsage struct {
	Messages int64 `json:"messages"`
	Bytes    int64 `json:"bytes"`
}

// InboxEnded counts Messages that ended without delivery in the last 24h.
type InboxEnded struct {
	Expired   int64 `json:"expired"`
	Replaced  int64 `json:"replaced"`
	Withdrawn int64 `json:"withdrawn"`
	Discarded int64 `json:"discarded"`
}

// InboxSummary is an inbox's state without its Messages: in `a12s status`
// and at the head of a list.
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
	// Orphaned is true when the Fleet no longer declares the Computer.
	Orphaned bool `json:"orphaned,omitempty"`
	// SessionOverride is "new" while `inbox resume --new` waits for the next Batch.
	SessionOverride string `json:"sessionOverride,omitempty"`
}

// InboxListResponse is GET /v1/computers/{c}/inbox.
type InboxListResponse struct {
	InboxSummary
	Messages []Message `json:"messages"`
}

// ClearRequest is POST /v1/computers/{c}/inbox/clear.
type ClearRequest struct {
	// ThroughSeq bounds the clear; nil means the current head.
	ThroughSeq *int64 `json:"throughSeq,omitempty"`
	// Orphan allows clearing the inbox of a Computer no longer in the Fleet.
	Orphan bool `json:"orphan,omitempty"`
}

// ClearResponse reports how many pending Messages were discarded.
type ClearResponse struct {
	Discarded int `json:"discarded"`
}

// HoldRequest is POST /v1/computers/{c}/inbox/hold.
type HoldRequest struct {
	Reason string `json:"reason,omitempty"`
}

// ResumeRequest is POST /v1/computers/{c}/inbox/resume.
type ResumeRequest struct {
	Session string `json:"session,omitempty"` // SessionCurrent (default) or SessionNew
}

// Hold reasons (§4.6).
const (
	HoldOperator      = "operator"
	HoldUndeliverable = "undeliverable"
	HoldResumeFailed  = "resume_failed"
	HoldOversized     = "oversized"
	HoldFailed        = "failed"
)

// Token actions (§4.4): what a scoped API token may do.
const (
	ActionSend   = "send"   // enqueue; get and withdraw own Messages
	ActionInbox  = "inbox"  // list, read bodies, clear, hold, resume
	ActionStatus = "status" // GET /v1/status
	ActionEvents = "events" // read a Computer's journal stream
	ActionPower  = "power"  // sleep and wake
	ActionStop   = "stop"   // interrupt a Turn
)

// TokenActions are the actions a scoped token may carry.
var TokenActions = []string{ActionSend, ActionInbox, ActionStatus, ActionEvents, ActionPower, ActionStop}

// TokenRequest is POST /v1/tokens (owner only).
type TokenRequest struct {
	Name      string   `json:"name"`
	Actions   []string `json:"actions"`
	Computers []string `json:"computers"` // ["*"] for every Computer
}

// Token describes a scoped token. Secret is only returned once, on create.
type Token struct {
	Name      string    `json:"name"`
	Actions   []string  `json:"actions"`
	Computers []string  `json:"computers"`
	CreatedAt time.Time `json:"createdAt"`
	Secret    string    `json:"secret,omitempty"`
}

// TokensResponse is GET /v1/tokens.
type TokensResponse struct {
	Tokens []Token `json:"tokens"`
}

// Power policy autoWake values (enhancements §4.14).
const (
	AutoWakeOn          = "on"
	AutoWakeOff         = "off"
	AutoWakeSuppressed  = "suppressed"   // manually slept: until a manual wake
	AutoWakePaused      = "paused"       // credentials not loaded, or the last wake failed
	AutoWakeRateLimited = "rate_limited" // maxWakesPerHour reached
)

// Slept-by values.
const (
	SleptAuto      = "auto"
	SleptManual    = "manual"
	SleptAllowWake = "allow_wake"
)

// PowerStatus is a Computer's power policy and its live state, in status.
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
