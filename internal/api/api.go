// Package api holds the JSON types of the CLI <-> Controller API and of
// computerd's event stream. Types only.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coltonkirsten/a12s/internal/fleet"
)

// ControllerFile is ~/.a12s/<fleet>/controller.json, mode 0600 because it
// holds the bearer token.
type ControllerFile struct {
	Port  int    `json:"port"`  // on 127.0.0.1
	Token string `json:"token"` // Authorization: Bearer <token>
}

// Credentials is keyed by credential name ("anthropic", "github", ...). The Controller keeps the values in its
// state volume (0600) unless it runs with memory-only credentials, and never returns them.
type Credentials map[string][]byte

// CredentialRequest is PUT /v1/credentials/{name}.
type CredentialRequest struct {
	Value string `json:"value"`
}

// CredentialInfo names a credential, never its value: whether it is set and who uses it.
type CredentialInfo struct {
	Name   string   `json:"name"`
	Set    bool     `json:"set"`
	UsedBy []string `json:"usedBy,omitempty"`
	// UpdatedAt is when the value last changed; absent if unknown.
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

type CredentialsResponse struct {
	Credentials []CredentialInfo `json:"credentials"`
}

// PutFleetRequest is PUT /v1/fleet: `a12s up` and fleet.replace. If-Match carries the version it read.
type PutFleetRequest struct {
	Fleet       *fleet.Spec `json:"fleet"`
	Lock        *fleet.Lock `json:"lock"`
	Credentials Credentials `json:"credentials,omitempty"`
	// Asleep creates new agent computers asleep.
	Asleep bool `json:"asleep,omitempty"`
	// Refresh names Services whose ref is resolved again and re-pinned.
	Refresh []string `json:"refresh,omitempty"`
	// File marks the Fleet as read from a Fleet file (a12s up): later API changes are drift from it.
	File bool `json:"file,omitempty"`
}

// PutFleetResponse answers every write to the Fleet: PUT /v1/fleet and the per-agent-computer and per-Service
// routes.
type PutFleetResponse struct {
	// Version is the Fleet's version after the write (unchanged by a dry run or a write that changed nothing).
	Version int64    `json:"version"`
	DryRun  bool     `json:"dryRun,omitempty"`
	Changes []Change `json:"changes"`
	Created []string `json:"created"`
	Updated []string `json:"updated"` // applied now (the agent computer was idle or asleep)
	Staged  []string `json:"staged"`  // waiting for the agent computer's next idle boundary
	Removed []string `json:"removed"` // stopped and deleted; drives kept
	// Lock is written back to fleet.lock by the CLI.
	Lock *fleet.Lock `json:"lock"`
	// Errors maps an agent computer or Service that did not converge to why; the rest was still applied.
	Errors   map[string]string `json:"errors,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
	// Services failures are in Errors.
	Services ServiceChanges `json:"services"`
	// AgentComputer or Service is the written one, after a PUT or PATCH of it.
	AgentComputer *AgentComputer `json:"agentComputer,omitempty"`
	Service       *ServiceStatus `json:"service,omitempty"`
}

// Change is one entry of a write's change list.
type Change struct {
	Op   string `json:"op"`   // ChangeCreate, ChangeUpdate or ChangeRemove
	Kind string `json:"kind"` // KindAgentComputer or KindService
	Name string `json:"name"`
	// When is "now", or "idle" for an update that waits for a busy agent computer's next idle boundary.
	When string `json:"when"`
	// Fields are the top-level spec fields that differ, for an update.
	Fields []string `json:"fields,omitempty"`
	// Diff is every leaf that differs, for an update.
	Diff []FieldDiff `json:"diff,omitempty"`
}

// FieldDiff is one leaf of a spec that differs: Path is dotted JSON field names (objects are walked, lists and
// values are leaves); Before or After is absent where the field is.
type FieldDiff struct {
	Path   string          `json:"path"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

const (
	ChangeCreate = "create"
	ChangeUpdate = "update"
	ChangeRemove = "remove"

	KindAgentComputer = "agentComputer"
	KindService       = "service"

	WhenNow  = "now"
	WhenIdle = "idle"
)

// FleetResponse is GET /v1/fleet; the ETag header is the version too.
type FleetResponse struct {
	Version int64      `json:"version"`
	Fleet   fleet.Spec `json:"fleet"`
	// Drift lists what changed through the API since the last whole-Fleet write (a12s up): an up from the file
	// undoes it.
	Drift []Change `json:"drift"`
}

// AgentComputer is GET /v1/agent-computers/{c}: its spec as written, with the defaults merged in, and its status.
type AgentComputer struct {
	Name      string          `json:"name"`
	Spec      json.RawMessage `json:"spec"`
	Effective json.RawMessage `json:"effective"`
	Status    ComputerStatus  `json:"status"`
}

type AgentComputersResponse struct {
	AgentComputers []AgentComputer `json:"agentComputers"`
}

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

type Failure struct {
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
}

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
	AgentComputers    []ComputerStatus `json:"agentComputers"`
	// Computers is AgentComputers under its old name, for one release.
	Computers      []ComputerStatus `json:"computers"`
	OrphanedDrives []DriveRow       `json:"orphanedDrives,omitempty"`
	// CredentialsLoaded is false after a Controller restart until `a12s up`
	// or `a12s wake` sends credentials again.
	CredentialsLoaded bool            `json:"credentialsLoaded"`
	Warnings          []string        `json:"warnings,omitempty"`
	OrphanedInboxes   []InboxSummary  `json:"orphanedInboxes,omitempty"`
	Services          []ServiceStatus `json:"services,omitempty"`
}

type ComputerStatus struct {
	Name          string   `json:"name"`
	Phase         Phase    `json:"phase"`
	Error         *Failure `json:"error,omitempty"`
	ResumePending bool     `json:"resumePending,omitempty"`
	Session       string   `json:"session,omitempty"`
	ActiveTurn    string   `json:"activeTurn,omitempty"`
	Harness       Harness  `json:"harness"`
	Image         string   `json:"image"` // slim or an image reference
	// Seccomp is the digest of the custom seccomp profile the agent computer runs with.
	Seccomp    string `json:"seccomp,omitempty"`
	Autonomy   string `json:"autonomy"`
	Generation int64  `json:"generation"`
	// StagedGeneration is greater than Generation while a change waits for idle.
	StagedGeneration     int64       `json:"stagedGeneration,omitempty"`
	CredentialGeneration int64       `json:"credentialGeneration,omitempty"`
	HomeSize             fleet.Bytes `json:"homeSize,omitzero"`
	Drives               []string    `json:"drives,omitempty"`
	// EgressMode is EgressEnforced, EgressUnenforced or EgressAllowAll, for
	// the active generation.
	EgressMode string        `json:"egressMode,omitempty"`
	Warnings   []string      `json:"warnings,omitempty"`
	Inbox      *InboxSummary `json:"inbox,omitempty"`
	// InboxCleared is set only on a wake --clear-inbox answer.
	InboxCleared *int         `json:"inboxCleared,omitempty"`
	Power        *PowerStatus `json:"power,omitempty"`
}

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

// TurnRequest is passed through to computerd. Repeating RequestID with the
// same body returns the original turn; a different body is a 409.
type TurnRequest struct {
	RequestID string     `json:"requestId"`
	Session   string     `json:"session"` // SessionCurrent, SessionNew or a session id
	Prompt    string     `json:"prompt"`
	Cwd       string     `json:"cwd,omitempty"`
	Inbox     *TurnInbox `json:"inbox,omitempty"`
	// Continuation runs computerd's owed restart_harness continuation.
	Continuation bool `json:"continuation,omitempty"`
	// LookupOnly returns an existing turn or refuses the requestId, never
	// starting one.
	LookupOnly bool `json:"lookupOnly,omitempty"`
}

// TurnResponse is returned only once the turn is durably accepted.
type TurnResponse struct {
	TurnID    string `json:"turnId"`
	SessionID string `json:"sessionId"`
}

type Power string

const (
	PowerAwake  Power = "awake"
	PowerAsleep Power = "asleep"
)

// PowerRequest carries credentials because waking re-injects them.
type PowerRequest struct {
	State       Power       `json:"state"`
	Credentials Credentials `json:"credentials,omitempty"`
	// ClearInbox discards pending Messages up to the head at acceptance;
	// RequestID makes a retry never clear a Message that arrived later.
	ClearInbox bool   `json:"clearInbox,omitempty"`
	RequestID  string `json:"requestId,omitempty"`
	// Hold, on a sleep, keeps it asleep until a manual wake, whatever its power policy says.
	Hold bool `json:"hold,omitempty"`
}

type ApplyRequest struct {
	Interrupt bool     `json:"interrupt"`
	Grace     Duration `json:"grace,omitzero"`
}

type ApplyResponse struct {
	Applied []string `json:"applied"`
	Pending []string `json:"pending"`
}

// ShellRequest lets the Controller journal the shell before the CLI runs
// `docker exec` itself.
type ShellRequest struct {
	Root    bool     `json:"root,omitempty"`
	Command []string `json:"command,omitempty"`
}

type ShellResponse struct {
	Container string `json:"container"`
	User      string `json:"user"` // "0:0" or "1000:1000"
}

type DrivesResponse struct {
	Drives []DriveRow `json:"drives"`
}

type DriveKind string

const (
	DriveHome  DriveKind = "home"
	DriveWork  DriveKind = "work"
	DriveState DriveKind = "state" // a Service's state drive: "<svc>-state"
)

// DriveRow.Name is what `a12s drive rm` takes: the drive name for work
// drives, "home-<computer>" for home drives.
type DriveRow struct {
	Name       string      `json:"name"`
	Kind       DriveKind   `json:"kind"`
	Computer   string      `json:"computer,omitempty"` // owner of a home drive
	Service    string      `json:"service,omitempty"`  // owner of a state drive
	Size       fleet.Bytes `json:"size,omitzero"`
	Access     string      `json:"access,omitempty"` // work drives
	AttachedTo []string    `json:"attachedTo,omitempty"`
	Orphaned   bool        `json:"orphaned,omitempty"`
}

type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Fix     string    `json:"fix,omitempty"`
	// Details carries structured fields (reason, id, replacedBy); Message
	// never depends on it.
	Details map[string]string `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Fix != "" {
		return fmt.Sprintf("%s: %s\n  fix: %s", e.Code, e.Message, e.Fix)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ErrorCode Busy (409) and Asleep (503) are passed through from computerd.
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

	ErrBodyTooLarge        ErrorCode = "body_too_large"       // 413
	ErrExceedsInbox        ErrorCode = "exceeds_inbox"        // 413: one Message bigger than maxBytes
	ErrBadMetadata         ErrorCode = "bad_metadata"         // 400
	ErrBadExpires          ErrorCode = "bad_expires"          // 400
	ErrInboxFull           ErrorCode = "inbox_full"           // 429
	ErrIdempotencyConflict ErrorCode = "idempotency_conflict" // 409
	ErrNotIdle             ErrorCode = "not_idle"             // 409: expires 0 and the Computer can't take it now
	ErrForbidden           ErrorCode = "forbidden"            // 403
	ErrNoGrant             ErrorCode = "no_grant"             // 403: send without grants.send
	ErrGone                ErrorCode = "gone"                 // 410
	ErrUnknownRequest      ErrorCode = "unknown_request"      // 404
	// Withdraw of a Message that is no longer pending: 409 with its state.
	ErrAlreadyClaimed   ErrorCode = "already_claimed"
	ErrAlreadyDelivered ErrorCode = "already_delivered"
	ErrExpired          ErrorCode = "expired"
	ErrReplaced         ErrorCode = "replaced"
	ErrWithdrawn        ErrorCode = "withdrawn"
	ErrDiscarded        ErrorCode = "discarded"

	ErrMissingField      ErrorCode = "missing_field"      // 400: e.g. no harness in the spec or the defaults
	ErrMissingCredential ErrorCode = "missing_credential" // 400: a credential the spec uses has no value
	ErrOutOfPrefix       ErrorCode = "out_of_prefix"      // 403: a name, or something it points at, outside the token's prefix
	ErrExists            ErrorCode = "exists"             // 409: create on a name that is taken
	ErrVersionMismatch   ErrorCode = "version_mismatch"   // 412: If-Match on a whole-Fleet write is stale
	ErrInUse             ErrorCode = "in_use"             // 409: deleting a credential that is still used
)

// Duration is a time.Duration written as a Go duration string ("30s").
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalWire skips HTML escaping so '<', '>' and '&' stay one byte each and
// a Batch's size on the wire is what the inbox reserved.
func MarshalWire(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
