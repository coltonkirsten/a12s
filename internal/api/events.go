package api

import (
	"encoding/json"
	"time"
)

// EventVersion is the envelope version, Event.V.
const EventVersion = 1

// Event is the computerd event envelope (§5). Seq is the SSE id and the
// journal sequence; Session and Turn are empty for events outside a turn.
type Event struct {
	V        int             `json:"v"`
	Computer string          `json:"computer"`
	Session  string          `json:"session"`
	Turn     string          `json:"turn"`
	Seq      int64           `json:"seq"`
	TS       time.Time       `json:"ts"`
	Type     EventType       `json:"type"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// EventType is an event's type.
type EventType string

// Event types (§5, plus harness.restarted).
const (
	EventTurnStarted      EventType = "turn.started"
	EventTextDelta        EventType = "text.delta"
	EventToolStarted      EventType = "tool.started"
	EventToolFinished     EventType = "tool.finished"
	EventPermission       EventType = "permission"
	EventDiagnostic       EventType = "diagnostic"
	EventTurnFinished     EventType = "turn.finished"
	EventSessionChanged   EventType = "session.changed"
	EventResumeFailed     EventType = "resume.failed"
	EventLifecycle        EventType = "lifecycle"
	EventHarnessRestarted EventType = "harness.restarted"
	// Enhancements §4.5, §4.6, §4.11.
	EventTurnDispatched        EventType = "turn.dispatched"
	EventContinuationAbandoned EventType = "continuation.abandoned"
	EventTurnRefused           EventType = "turn.refused"
	EventInboxDiscarded        EventType = "inbox.discarded"
)

// SSEGap is the SSE event name sent instead of events when Last-Event-ID is
// unknown or ahead of the journal.
const SSEGap = "gap"

// SSEHead is the SSE event name computerd sends once, right after replaying
// the journal on a new connection: everything up to HeadData.Seq has been
// sent. What follows is live.
const SSEHead = "head"

// HeadData is the data of an SSEHead event.
type HeadData struct {
	Seq int64 `json:"seq"`
}

// The Data payloads below mirror what computerd/src/computer.ts and its
// adapters emit; fields computerd adds later are ignored.

// TurnStartedData is Data of turn.started.
type TurnStartedData struct {
	Harness Harness `json:"harness"` // kind and version
	Model   string  `json:"model,omitempty"`
	Resumed bool    `json:"resumed"`
	Prompt  string  `json:"prompt"`
	Cwd     string  `json:"cwd"`
	// RequestID is the turn's requestId: a Batch id, a continuation id or
	// a client's id.
	RequestID string `json:"requestId,omitempty"`
	// Inbox describes the Batch the turn carries (enhancements §4.5).
	Inbox *TurnInbox `json:"inbox,omitempty"`
	// Continuation is set on the restart_harness continuation turn.
	Continuation string `json:"continuation,omitempty"`
}

// TurnInbox is the Batch a turn carries, copied by computerd into
// turn.started so a client can find the turn that carries its Message.
type TurnInbox struct {
	Batch        string         `json:"batch"`
	PendingAfter int            `json:"pendingAfter"`
	Messages     []TurnInboxMsg `json:"messages"`
}

// TurnInboxMsg is one Message of a Batch, without its body.
type TurnInboxMsg struct {
	ID         string    `json:"id"`
	Seq        int64     `json:"seq"`
	Sender     string    `json:"sender"`
	Via        string    `json:"via"`
	CreatedAt  time.Time `json:"createdAt"`
	Bytes      int64     `json:"bytes"`
	Key        string    `json:"key,omitempty"`
	Superseded *int64    `json:"superseded,omitempty"`
}

// TurnSummary is computerd's GET /v1/turns/{id}: what settlement needs.
type TurnSummary struct {
	TurnID       string `json:"turnId"`
	RequestID    string `json:"requestId"`
	Dispatched   bool   `json:"dispatched"`
	ResumeFailed bool   `json:"resumeFailed"`
	Finished     string `json:"finished,omitempty"` // "" while running
	Error        string `json:"error,omitempty"`
	Session      string `json:"session,omitempty"`
}

// TextDeltaData is Data of text.delta.
type TextDeltaData struct {
	Text string `json:"text"`
}

// Turn outcomes in TurnFinishedData.Status.
const (
	TurnCompleted   = "completed"
	TurnFailed      = "failed"
	TurnInterrupted = "interrupted"
)

// TurnFinishedData is Data of turn.finished. Usage is the harness's own
// usage object, or null.
type TurnFinishedData struct {
	Status string          `json:"status"`
	Usage  json.RawMessage `json:"usage"`
	Error  string          `json:"error,omitempty"`
}

// DiagnosticData is Data of diagnostic: a message plus event-specific fields
// (names, source, ...). As the body of computerd's POST /v1/diagnostics it is
// {message, data}; computerd journals that with source "controller".
type DiagnosticData struct {
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	Source  string          `json:"source,omitempty"`
}

// Where a resume failed, in ResumeFailedData.At.
const (
	ResumeAtWake    = "wake"
	ResumeAtPrompt  = "prompt"
	ResumeAtRestart = "restart"
)

// ResumeFailedData is Data of resume.failed.
type ResumeFailedData struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason"`
	At        string `json:"at"` // ResumeAtWake, ResumeAtPrompt or ResumeAtRestart
}

// ComputerdStatus is computerd's GET /v1/status.
type ComputerdStatus struct {
	Phase                string  `json:"phase"`
	Session              string  `json:"session,omitempty"`
	ActiveTurn           string  `json:"activeTurn,omitempty"`
	Harness              Harness `json:"harness"`
	CredentialGeneration int64   `json:"credentialGeneration"`
	ResumePending        bool    `json:"resumePending,omitempty"`
	// Continuation is the restart_harness continuation computerd owes; the
	// Controller runs it at the next idle boundary.
	Continuation string `json:"continuation,omitempty"`
}
