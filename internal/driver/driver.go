// Package driver is the substrate interface, scoped to one Fleet. Every call is
// idempotent, every mutation carries a generation, and success is read back through Observe.
package driver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
)

type Driver interface {
	// Gaps in Capabilities become admission errors in the reconciler.
	Capabilities(ctx context.Context) (Capabilities, error)

	Observe(ctx context.Context) (Observation, error)

	// Watch streams Computer failures until ctx ends, then closes the channel.
	Watch(ctx context.Context) (<-chan Event, error)

	// EnsureDrive never shrinks or erases an existing drive.
	EnsureDrive(ctx context.Context, spec DriveSpec) (created bool, err error)

	// EnsureImage pulls only if missing; ref may be a reference or an image ID.
	EnsureImage(ctx context.Context, ref string) (imageID string, err error)

	// InspectComputerImage reports what admission checks on a custom Computer image; imageID is local.
	InspectComputerImage(ctx context.Context, imageID string) (ComputerImage, error)

	// EnsureBundle exposes the bundle read-only to Computers at BundlePath.
	EnsureBundle(ctx context.Context, imageRef string) (Bundle, error)

	// StageComputer never touches a running Computer (ErrRunning) and rejects
	// a generation older than the staged one (ErrStaleGeneration).
	StageComputer(ctx context.Context, spec ComputerSpec) error

	// SetPower acts only on the staged generation; others get ErrStaleGeneration or ErrNotStaged.
	SetPower(ctx context.Context, computer string, power Power, generation int64) error

	// Connect is unauthenticated; the caller layers mTLS on top.
	Connect(ctx context.Context, computer string, service string) (net.Conn, error)

	// Exec runs outside the harness (audited debug and credential injection).
	// Output is stdout and stderr combined.
	Exec(ctx context.Context, computer string, cmd []string, stdin io.Reader, asRoot bool) (exitCode int, output string, err error)

	// RemoveComputer keeps the Computer's drives.
	RemoveComputer(ctx context.Context, computer string) error

	// RemoveDrive refuses with ErrDriveAttached while anything has it attached.
	RemoveDrive(ctx context.Context, id string) error

	// Sessions counts attached interactive/exec sessions; any keeps the Computer from auto-sleeping.
	Sessions(ctx context.Context, computer string) (int, error)

	// Caller names the Computer or Service an egress proxy connection comes from, judged by whichever of the
	// connection's local or remote address the substrate makes unforgeable.
	Caller(local, remote netip.Addr) (name string, ok bool)

	ServiceDriver
}

var (
	ErrNotFound        = errors.New("not found")
	ErrRunning         = errors.New("computer is running")
	ErrStaleGeneration = errors.New("stale generation")
	ErrNotStaged       = errors.New("generation not staged")
	ErrDriveAttached   = errors.New("drive is attached")
	ErrNotOnNetwork    = errors.New("caller is not on the computer's network")
)

const ServiceComputerd = "computerd"

// ComputerdMain is computerd's entrypoint in every Computer image.
const ComputerdMain = "/opt/a12s/computerd/dist/main.js"

// LabelComputerdVersion is set on the a12s base images and inherited by every image built FROM them.
const LabelComputerdVersion = "ai.a12s.computerd.version"

type ComputerImage struct {
	Labels    map[string]string
	Computerd bool // ComputerdMain exists
}

const ComputerdPort = 7443

type Power string

const (
	PowerAwake  Power = "awake"
	PowerAsleep Power = "asleep"
)

type Capabilities struct {
	EgressEnforcement bool     `json:"egressEnforcement"`
	DriveQuota        bool     `json:"driveQuota"`
	Architectures     []string `json:"architectures"` // GOARCH names: "arm64", "amd64"
	SandboxedRuntime  bool     `json:"sandboxedRuntime"`
	// A multi-node Driver reports Services false until it encrypts both hops.
	Services bool `json:"services"`
	// HostFolders: the substrate can bind a folder of the owner's host into a Computer (attach).
	HostFolders bool `json:"hostFolders"`
}

// Host is how the Controller is reached on its substrate.
type Host struct {
	// ControllerName is in the Controller's server certificate, and Computers dial the Controller by it.
	ControllerName string
	// APIAddr is where the owner API listens unless configured otherwise.
	APIAddr string
	// RemoteEngines: Computers' containers run in engines other than the one on the Controller's host, so the CLI
	// can't docker exec into them (a12s shell).
	RemoteEngines bool
}

type Observation struct {
	Computers map[string]ComputerState `json:"computers"`
	Drives    map[string]DriveState    `json:"drives"`
	Services  map[string]ComputerState `json:"services,omitempty"`
}

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
	// Identity is the cloud principal the Computer acts as, for the app to grant access to; empty without one.
	Identity string `json:"identity,omitempty"`
}

type DriveState struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"` // substrate object name
	AttachedTo []string `json:"attachedTo,omitempty"`
	// Computer and Path are a keep drive's owner and mount point, as it was created.
	Computer string `json:"computer,omitempty"`
	Path     string `json:"path,omitempty"`
}

type EventType string

const (
	EventDied    EventType = "died"
	EventOOM     EventType = "oom"
	EventRemoved EventType = "removed"
)

type Event struct {
	Type       EventType `json:"type"`
	Computer   string    `json:"computer"`
	Generation int64     `json:"generation"`
	ExitCode   int       `json:"exitCode,omitempty"`
	// When Service is set, Computer holds the Service's name.
	Service bool `json:"service,omitempty"`
}
