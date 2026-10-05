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
)

// SSEGap is the SSE event name sent instead of events when Last-Event-ID is
// unknown or ahead of the journal.
const SSEGap = "gap"

// TurnStartedData is Data of turn.started.
type TurnStartedData struct {
	HarnessVersion string `json:"harnessVersion"`
	Model          string `json:"model,omitempty"`
	Resumed        bool   `json:"resumed"`
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

// TurnFinishedData is Data of turn.finished.
type TurnFinishedData struct {
	Status string          `json:"status"`
	Error  string          `json:"error,omitempty"`
	Usage  json.RawMessage `json:"usage,omitempty"`
}

// DiagnosticData is Data of diagnostic, and the body of computerd's
// POST /v1/diagnostics (the Controller reports denied egress this way).
type DiagnosticData struct {
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// ResumeFailedData is Data of resume.failed.
type ResumeFailedData struct {
	Session string `json:"session"`
	Reason  string `json:"reason"`
}

// ComputerdStatus is computerd's GET /v1/status.
type ComputerdStatus struct {
	Phase                string  `json:"phase"`
	Session              string  `json:"session,omitempty"`
	ActiveTurn           string  `json:"activeTurn,omitempty"`
	Harness              Harness `json:"harness"`
	CredentialGeneration int64   `json:"credentialGeneration"`
	ResumePending        bool    `json:"resumePending,omitempty"`
}
