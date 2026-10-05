// Package driver defines the substrate interface the Controller's reconciler
// drives (§4). A Driver is scoped to one Fleet. It holds no opinions: every
// call is idempotent, every mutation carries a generation, and success is
// read back through Observe.
package driver

import (
	"context"
	"errors"
	"io"
	"net"
)

// Driver is a substrate backend: Docker now, Kubernetes later.
type Driver interface {
	// Capabilities reports what the substrate can enforce. Gaps become
	// admission errors in the reconciler.
	Capabilities(ctx context.Context) (Capabilities, error)

	// Observe returns every Computer and Drive of the Fleet as the substrate
	// sees them now.
	Observe(ctx context.Context) (Observation, error)

	// Watch streams Computer failures (died, OOM-killed, removed) until ctx
	// ends. The channel is closed when the watch stops.
	Watch(ctx context.Context) (<-chan Event, error)

	// EnsureDrive creates the drive if it does not exist. It never shrinks or
	// erases a drive. It reports whether the drive was created.
	EnsureDrive(ctx context.Context, spec DriveSpec) (created bool, err error)

	// EnsureBundle makes the HarnessBundle image available read-only to
	// Computers at BundlePath(kind, version). It is idempotent and returns the
	// image ID and the bundle's manifest.
	EnsureBundle(ctx context.Context, imageRef string) (Bundle, error)

	// StageComputer prepares spec.Generation's definition so SetPower can start
	// it. It returns ErrRunning rather than touch a running Computer, and
	// ErrStaleGeneration if a newer generation is already staged.
	StageComputer(ctx context.Context, spec ComputerSpec) error

	// SetPower starts (PowerAwake) or stops (PowerAsleep) the staged
	// generation. A generation other than the staged one is rejected with
	// ErrStaleGeneration or ErrNotStaged.
	SetPower(ctx context.Context, computer string, power Power, generation int64) error

	// Connect opens a channel to a service inside the Computer (ServiceComputerd).
	// Authentication (mTLS) is layered on top by the caller.
	Connect(ctx context.Context, computer string, service string) (net.Conn, error)

	// Exec runs cmd in a running Computer, outside the harness: the audited
	// debug path and the credential-inject path. stdin may be nil. Output is
	// stdout and stderr combined.
	Exec(ctx context.Context, computer string, cmd []string, stdin io.Reader, asRoot bool) (exitCode int, output string, err error)

	// RemoveComputer stops and deletes the Computer. Its drives are kept.
	RemoveComputer(ctx context.Context, computer string) error

	// RemoveDrive destroys a drive. It returns ErrDriveAttached if anything
	// has it attached.
	RemoveDrive(ctx context.Context, id string) error
}

// Errors returned by Drivers; test with errors.Is.
var (
	ErrNotFound        = errors.New("not found")
	ErrRunning         = errors.New("computer is running")
	ErrStaleGeneration = errors.New("stale generation")
	ErrNotStaged       = errors.New("generation not staged")
	ErrDriveAttached   = errors.New("drive is attached")
	ErrNotOnNetwork    = errors.New("caller is not on the computer's network")
)

// ServiceComputerd is computerd's mTLS listener (§5).
const ServiceComputerd = "computerd"

// ComputerdPort is the port computerd listens on.
const ComputerdPort = 7443

// Power is a Computer's power state.
type Power string

const (
	PowerAwake  Power = "awake"
	PowerAsleep Power = "asleep"
)

// Capabilities is what a substrate can enforce.
type Capabilities struct {
	SharedWriteDrives bool     `json:"sharedWriteDrives"`
	EgressEnforcement bool     `json:"egressEnforcement"`
	DriveQuota        bool     `json:"driveQuota"`
	Architectures     []string `json:"architectures"` // GOARCH names: "arm64", "amd64"
	SandboxedRuntime  bool     `json:"sandboxedRuntime"`
}

// Observation is the substrate's view of one Fleet.
type Observation struct {
	Computers map[string]ComputerState `json:"computers"`
	Drives    map[string]DriveState    `json:"drives"`
}

// ComputerState is one Computer as observed.
type ComputerState struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"` // the staged generation
	Power      Power  `json:"power"`      // awake once started, until stopped or exited
	Running    bool   `json:"running"`    // the main process is running now
	InstanceID string `json:"instanceId"` // container ID or pod UID
	Network    string `json:"network"`
	Address    string `json:"address,omitempty"` // IP on its own network while running
	ExitCode   int    `json:"exitCode,omitempty"`
	OOMKilled  bool   `json:"oomKilled,omitempty"`
	// Failure explains an abnormal stop (OOM, runtime error, non-zero exit). Empty otherwise.
	Failure string `json:"failure,omitempty"`
}

// DriveState is one drive as observed.
type DriveState struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"` // substrate object name
	AttachedTo []string `json:"attachedTo,omitempty"`
}

// EventType is a Computer failure kind reported by Watch.
type EventType string

const (
	EventDied    EventType = "died"
	EventOOM     EventType = "oom"
	EventRemoved EventType = "removed"
)

// Event is one Computer failure.
type Event struct {
	Type       EventType `json:"type"`
	Computer   string    `json:"computer"`
	Generation int64     `json:"generation"`
	ExitCode   int       `json:"exitCode,omitempty"`
}
