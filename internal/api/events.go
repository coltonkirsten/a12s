package api

import (
	"encoding/json"
	"time"
)

const EventVersion = 1

// Event.Seq is the agent computer's journal sequence (0 for events the Controller makes); Session and Turn are
// empty for events outside a turn.
type Event struct {
	V int `json:"v"`
	// AgentComputer is empty on fleet.changed and service.phase.
	AgentComputer string          `json:"agentComputer,omitempty"`
	Session       string          `json:"session"`
	Turn          string          `json:"turn"`
	Seq           int64           `json:"seq"`
	TS            time.Time       `json:"ts"`
	Type          EventType       `json:"type"`
	Data          json.RawMessage `json:"data,omitempty"`
}

type EventType string

const (
	EventTurnStarted           EventType = "turn.started"
	EventTextDelta             EventType = "text.delta"
	EventToolStarted           EventType = "tool.started"
	EventToolFinished          EventType = "tool.finished"
	EventPermission            EventType = "permission"
	EventDiagnostic            EventType = "diagnostic"
	EventTurnFinished          EventType = "turn.finished"
	EventSessionChanged        EventType = "session.changed"
	EventResumeFailed          EventType = "resume.failed"
	EventLifecycle             EventType = "lifecycle"
	EventHarnessRestarted      EventType = "harness.restarted"
	EventTurnDispatched        EventType = "turn.dispatched"
	EventContinuationAbandoned EventType = "continuation.abandoned"
	EventTurnRefused           EventType = "turn.refused"
	EventInboxDiscarded        EventType = "inbox.discarded"
	EventEgressDenied          EventType = "egress.denied"

	// Made by the Controller, on GET /v1/events only.
	EventFleetChanged EventType = "fleet.changed"
	EventPhase        EventType = "agent_computer.phase"
	// EventInboxChanged carries the inbox's InboxSummary.
	EventInboxChanged EventType = "inbox.changed"
	// EventServicePhase has no agentComputer: the Service is named in its data.
	EventServicePhase EventType = "service.phase"
)

// EgressDeniedData: the egress proxy refused a connection.
type EgressDeniedData struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Reason string `json:"reason,omitempty"`
}

// FleetChangedData: the Fleet's version moved; Names are what the write changed.
type FleetChangedData struct {
	Version int64    `json:"version"`
	By      string   `json:"by"`
	Names   []string `json:"names"`
}

// PhaseData: an agent computer's phase changed.
type PhaseData struct {
	Phase   Phase    `json:"phase"`
	Error   *Failure `json:"error,omitempty"`
	Waiting *Waiting `json:"waiting,omitempty"`
}

// ServicePhaseData: a Service's phase, health or restart count changed.
type ServicePhaseData struct {
	Service  string   `json:"service"`
	Phase    Phase    `json:"phase"`
	Health   string   `json:"health,omitempty"`
	Restarts int      `json:"restarts"`
	Error    *Failure `json:"error,omitempty"`
	Waiting  *Waiting `json:"waiting,omitempty"`
}

// SSEGap is sent instead of events when Last-Event-ID is unknown or ahead of
// the journal.
const SSEGap = "gap"

// SSEHead is sent once after the journal replay: everything up to
// HeadData.Seq has been sent, and what follows is live.
const SSEHead = "head"

type HeadData struct {
	Seq int64 `json:"seq"`
}

// The Data payloads below mirror computerd/src/computer.ts and its adapters;
// unknown fields are ignored.

type TurnStartedData struct {
	Harness Harness `json:"harness"` // kind and version
	Model   string  `json:"model,omitempty"`
	Resumed bool    `json:"resumed"`
	Prompt  string  `json:"prompt"`
	Cwd     string  `json:"cwd"`
	// RequestID is a Batch id, a continuation id or a client's id.
	RequestID    string     `json:"requestId,omitempty"`
	Inbox        *TurnInbox `json:"inbox,omitempty"`
	Continuation string     `json:"continuation,omitempty"`
}

// TurnInbox is copied by computerd into turn.started so a client can find
// the turn that carries its Message.
type TurnInbox struct {
	Batch        string         `json:"batch"`
	PendingAfter int            `json:"pendingAfter"`
	Messages     []TurnInboxMsg `json:"messages"`
}

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

type TurnSummary struct {
	TurnID       string `json:"turnId"`
	RequestID    string `json:"requestId"`
	Dispatched   bool   `json:"dispatched"`
	ResumeFailed bool   `json:"resumeFailed"`
	Finished     string `json:"finished,omitempty"` // "" while running
	Error        string `json:"error,omitempty"`
	Session      string `json:"session,omitempty"`
}

type TextDeltaData struct {
	Text string `json:"text"`
}

const (
	TurnCompleted   = "completed"
	TurnFailed      = "failed"
	TurnInterrupted = "interrupted"
)

// TurnFinishedData.Usage is the harness's own usage object, or null; Tokens is the same, normalized.
type TurnFinishedData struct {
	Status string          `json:"status"`
	Usage  json.RawMessage `json:"usage"`
	Tokens *Tokens         `json:"tokens,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Tokens is a Turn's token usage for every harness. Input excludes cached input.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

// Turn is a finished Turn folded from its events: GET /v1/agent-computers/{c}/turns, and what the SDKs' turns()
// yields.
type Turn struct {
	AgentComputer string         `json:"agentComputer"`
	ID            string         `json:"id"`
	Session       string         `json:"session"`
	Status        string         `json:"status"`
	Text          string         `json:"text"`
	Error         string         `json:"error,omitempty"`
	Messages      []TurnInboxMsg `json:"messages"`
	Tools         []ToolCall     `json:"tools"`
	// Usage is the harness's own, Tokens the normalized one.
	Usage      json.RawMessage `json:"usage"`
	Tokens     *Tokens         `json:"tokens,omitempty"`
	StartedAt  *time.Time      `json:"startedAt,omitempty"`
	FinishedAt time.Time       `json:"finishedAt"`
}

type ToolCall struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"`
	IsError bool            `json:"isError,omitempty"`
}

type TurnsResponse struct {
	Turns []Turn `json:"turns"`
}

// DiagnosticData is also the body of computerd's POST /v1/diagnostics, which
// journals it with source "controller".
type DiagnosticData struct {
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	Source  string          `json:"source,omitempty"`
}

const (
	ResumeAtWake    = "wake"
	ResumeAtPrompt  = "prompt"
	ResumeAtRestart = "restart"
)

type ResumeFailedData struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason"`
	At        string `json:"at"` // ResumeAtWake, ResumeAtPrompt or ResumeAtRestart
}

type ComputerdStatus struct {
	Phase                string  `json:"phase"`
	Session              string  `json:"session,omitempty"`
	ActiveTurn           string  `json:"activeTurn,omitempty"`
	Harness              Harness `json:"harness"`
	CredentialGeneration int64   `json:"credentialGeneration"`
	ResumePending        bool    `json:"resumePending,omitempty"`
	// Continuation is owed by computerd; the Controller runs it at the next
	// idle boundary.
	Continuation string `json:"continuation,omitempty"`
}
