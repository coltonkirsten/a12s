// Package api holds the JSON types of the CLI <-> Controller API (PLAN
// "Controller API") and of computerd's event stream (§5). Types only.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coltonkirsten/a12s/internal/fleet"
)

// ControllerFile is ~/.a12s/<fleet>/controller.json (mode 0600): where the
// CLI finds the Controller and the bearer token it must present.
type ControllerFile struct {
	Port  int    `json:"port"`  // on 127.0.0.1
	Token string `json:"token"` // Authorization: Bearer <token>
}

// Credentials maps a credential name ("anthropic", "openai", "github", ...)
// to its material, read by the CLI from the bindings file. []byte is base64
// in JSON. The Controller keeps it in memory only.
type Credentials map[string][]byte

// PutFleetRequest is PUT /v1/fleet: `a12s up`.
type PutFleetRequest struct {
	Fleet       *fleet.Fleet `json:"fleet"`
	Lock        *fleet.Lock  `json:"lock"`
	Credentials Credentials  `json:"credentials,omitempty"`
	// Asleep creates new Computers asleep (`a12s up --asleep`).
	Asleep bool `json:"asleep,omitempty"`
	// Presets holds each home preset as a tar stream (fleet.WritePresetTar),
	// keyed by the preset path exactly as written in the Fleet. The
	// Controller cannot read the host, so the CLI sends every preset the
	// Fleet names. []byte is base64 in JSON.
	Presets map[string][]byte `json:"presets,omitempty"`
}

// PutFleetResponse lists what applying the Fleet did, by Computer name.
type PutFleetResponse struct {
	Created []string `json:"created"`
	Updated []string `json:"updated"` // applied now (the Computer was idle or asleep)
	Staged  []string `json:"staged"`  // waiting for the Computer's next idle boundary
	Removed []string `json:"removed"` // stopped and deleted; drives kept
	// Lock is the lock after resolution; the CLI writes it back to fleet.lock.
	Lock *fleet.Lock `json:"lock"`
	// Errors maps a Computer to why it did not converge (it is in PhaseError).
	// The rest of the Fleet was still applied.
	Errors map[string]string `json:"errors,omitempty"`
	// Warnings are accepted but noteworthy settings (a large inbox maxBytes).
	Warnings []string `json:"warnings,omitempty"`
}

// Phase is a Computer's Controller state (§7).
type Phase string

const (
	PhaseProvisioning Phase = "Provisioning"
	PhaseStarting     Phase = "Starting"
	PhaseReady        Phase = "Ready"
	PhaseBusy         Phase = "Busy"
	PhaseStopping     Phase = "Stopping"
	PhaseAsleep       Phase = "Asleep"
	PhaseError        Phase = "Error"
)

// Failure explains PhaseError: why, and whether a retry can help.
type Failure struct {
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
}

// Harness is the harness a Computer runs.
type Harness struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Model   string `json:"model,omitempty"`
}

// StatusResponse is GET /v1/status.
type StatusResponse struct {
	Fleet string `json:"fleet"`
	// EgressEnforcement is "enforced" or "unenforced".
	EgressEnforcement string           `json:"egressEnforcement"`
	Computers         []ComputerStatus `json:"computers"`
	// OrphanedDrives are drives no Computer in the Fleet attaches.
	OrphanedDrives []DriveRow `json:"orphanedDrives,omitempty"`
	// CredentialsLoaded is false after a Controller restart until `a12s up`
	// or `a12s wake` sends credentials again.
	CredentialsLoaded bool `json:"credentialsLoaded"`
	// Warnings are Fleet-wide: unenforced egress, orphaned drives, missing credentials.
	Warnings []string `json:"warnings,omitempty"`
	// OrphanedInboxes are inboxes of Computers no longer in the Fleet.
	OrphanedInboxes []InboxSummary `json:"orphanedInboxes,omitempty"`
}

// ComputerStatus is one row of `a12s status`.
type ComputerStatus struct {
	Name          string   `json:"name"`
	Phase         Phase    `json:"phase"`
	Error         *Failure `json:"error,omitempty"`
	ResumePending bool     `json:"resumePending,omitempty"`
	Session       string   `json:"session,omitempty"`
	ActiveTurn    string   `json:"activeTurn,omitempty"`
	Harness       Harness  `json:"harness"`
	Image         string   `json:"image"` // tier or image reference
	Autonomy      string   `json:"autonomy"`
	Generation    int64    `json:"generation"`
	// StagedGeneration is greater than Generation while a change waits for idle.
	StagedGeneration     int64       `json:"stagedGeneration,omitempty"`
	CredentialGeneration int64       `json:"credentialGeneration,omitempty"`
	HomeSize             fleet.Bytes `json:"homeSize,omitzero"`
	Drives               []string    `json:"drives,omitempty"`
	// EgressMode is the egress mode the Controller enforces for the Computer's
	// active generation: EgressEnforced, EgressUnenforced or EgressAllowAll.
	EgressMode string `json:"egressMode,omitempty"`
	// Warnings are per Computer: egress allow "*", resume failed, and so on.
	Warnings []string `json:"warnings,omitempty"`
	// Inbox is the Computer's inbox state (enhancements §4).
	Inbox *InboxSummary `json:"inbox,omitempty"`
	// InboxCleared is set on a wake --clear-inbox answer: Messages discarded.
	InboxCleared *int `json:"inboxCleared,omitempty"`
	// Power is the declared power policy and its state (enhancements §4.14).
	Power *PowerStatus `json:"power,omitempty"`
}

// ComputerStatus.EgressMode values.
const (
	EgressEnforced   = "enforced"   // only the declared hosts, over CONNECT to 443
	EgressUnenforced = "unenforced" // the Fleet accepts unenforced egress: any public host
	EgressAllowAll   = "allow-all"  // allow: ["*"]: any public host on any port
)

// Session values for TurnRequest.Session besides an explicit session id.
const (
	SessionCurrent = "current"
	SessionNew     = "new"
)

// TurnRequest is POST /v1/computers/{c}/turns, passed through to computerd's
// POST /v1/turns. Repeating RequestID with the same body returns the
// original turn; a different body is a 409.
type TurnRequest struct {
	RequestID string `json:"requestId"`
	Session   string `json:"session"` // SessionCurrent, SessionNew or a session id
	Prompt    string `json:"prompt"`
	Cwd       string `json:"cwd,omitempty"`
	// Inbox describes the Batch (enhancements §4.5).
	Inbox *TurnInbox `json:"inbox,omitempty"`
	// Continuation runs computerd's owed restart_harness continuation.
	Continuation bool `json:"continuation,omitempty"`
	// LookupOnly returns an existing turn or refuses the requestId (§4.11).
	LookupOnly bool `json:"lookupOnly,omitempty"`
}

// TurnResponse is returned once the turn is durably accepted.
type TurnResponse struct {
	TurnID    string `json:"turnId"`
	SessionID string `json:"sessionId"`
}

// Power is a requested power state.
type Power string

const (
	PowerAwake  Power = "awake"
	PowerAsleep Power = "asleep"
)

// PowerRequest is POST /v1/computers/{c}/power. Waking re-injects
// credentials, so `a12s wake` sends them.
type PowerRequest struct {
	State       Power       `json:"state"`
	Credentials Credentials `json:"credentials,omitempty"`
	// ClearInbox discards the pending Messages up to the inbox head at
	// acceptance before admission opens (§4.7). RequestID makes it
	// idempotent: a retry never clears a Message that arrived later.
	ClearInbox bool   `json:"clearInbox,omitempty"`
	RequestID  string `json:"requestId,omitempty"`
	// AllowWake, on a sleep, leaves the power policy in charge (§4.14).
	AllowWake bool `json:"allowWake,omitempty"`
}

// ApplyRequest is POST /v1/apply: `a12s apply --interrupt --grace 30s`.
type ApplyRequest struct {
	Interrupt bool     `json:"interrupt"`
	Grace     Duration `json:"grace,omitzero"`
}

// ApplyResponse lists Computers whose staged change was applied, and those
// still waiting (busy, without --interrupt).
type ApplyResponse struct {
	Applied []string `json:"applied"`
	Pending []string `json:"pending"`
}

// ShellRequest is POST /v1/computers/{c}/shell: the Controller journals a
// lifecycle event and returns the container for the CLI to `docker exec`.
type ShellRequest struct {
	Root    bool     `json:"root,omitempty"`
	Command []string `json:"command,omitempty"`
}

// ShellResponse names the container to exec into and the user to exec as.
type ShellResponse struct {
	Container string `json:"container"`
	User      string `json:"user"` // "0:0" or "1000:1000"
}

// DrivesResponse is GET /v1/drives.
type DrivesResponse struct {
	Drives []DriveRow `json:"drives"`
}

// DriveKind is "home" or "work".
type DriveKind string

const (
	DriveHome DriveKind = "home"
	DriveWork DriveKind = "work"
)

// DriveRow is one row of `a12s drive ls`. Name is what `a12s drive rm` takes:
// the drive name for work drives, "home-<computer>" for home drives.
type DriveRow struct {
	Name       string      `json:"name"`
	Kind       DriveKind   `json:"kind"`
	Computer   string      `json:"computer,omitempty"` // owner of a home drive
	Size       fleet.Bytes `json:"size,omitzero"`
	Access     string      `json:"access,omitempty"` // work drives
	AttachedTo []string    `json:"attachedTo,omitempty"`
	// Orphaned is true when the Fleet no longer declares the drive or its Computer.
	Orphaned bool `json:"orphaned,omitempty"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	// Fix, when known, says how to resolve the error (enhancements §6.3).
	Fix string `json:"fix,omitempty"`
	// Details carries structured fields of some errors (reason, id,
	// replacedBy); never needed to read Message.
	Details map[string]string `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Fix != "" {
		return fmt.Sprintf("%s: %s\n  fix: %s", e.Code, e.Message, e.Fix)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ErrorCode classifies an Error. Busy is HTTP 409 and Asleep 503, passed
// through from computerd.
type ErrorCode string

const (
	ErrBusy         ErrorCode = "busy"     // 409
	ErrConflict     ErrorCode = "conflict" // 409: requestId reused with a different body
	ErrAsleep       ErrorCode = "asleep"   // 503
	ErrNotFound     ErrorCode = "not_found"
	ErrInvalid      ErrorCode = "invalid"
	ErrUnauthorized ErrorCode = "unauthorized"
	ErrAttached     ErrorCode = "attached" // drive rm while attached
	ErrInternal     ErrorCode = "internal"

	// Inbox errors (enhancements §4).
	ErrBodyTooLarge        ErrorCode = "body_too_large"       // 413
	ErrExceedsInbox        ErrorCode = "exceeds_inbox"        // 413: one Message bigger than maxBytes
	ErrBadMetadata         ErrorCode = "bad_metadata"         // 400
	ErrBadExpires          ErrorCode = "bad_expires"          // 400
	ErrInboxFull           ErrorCode = "inbox_full"           // 429
	ErrIdempotencyConflict ErrorCode = "idempotency_conflict" // 409
	ErrNotIdle             ErrorCode = "not_idle"             // 409: expires 0 and the Computer can't take it now
	ErrForbidden           ErrorCode = "forbidden"            // 403
	ErrGone                ErrorCode = "gone"                 // 410
	ErrUnknownRequest      ErrorCode = "unknown_request"      // 404
	// Withdraw of a Message that is no longer pending: 409 with its state.
	ErrAlreadyClaimed   ErrorCode = "already_claimed"
	ErrAlreadyDelivered ErrorCode = "already_delivered"
	ErrExpired          ErrorCode = "expired"
	ErrReplaced         ErrorCode = "replaced"
	ErrWithdrawn        ErrorCode = "withdrawn"
	ErrDiscarded        ErrorCode = "discarded"
)

// Duration is a time.Duration written as a Go duration string ("30s").
type Duration time.Duration

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalWire encodes v as JSON without HTML escaping: '<', '>' and '&'
// stay one byte each. The Controller sends Turn requests to computerd this
// way, so a Batch's rendered size on the wire is what the inbox reserved
// (enhancements §4.3).
func MarshalWire(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
