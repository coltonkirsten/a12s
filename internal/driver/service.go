package driver

import (
	"context"
	"io"
	"net"
	"strings"
)

// ServiceDriver is what a Driver offers for Services (enhancements §5): the
// same isolation as a Computer, with no computerd inside.
type ServiceDriver interface {
	// ServiceImage reads what the Controller needs from a pinned Service
	// image: /a12s/service.json (nil when absent) and its declared VOLUMEs.
	ServiceImage(ctx context.Context, imageID string) (ServiceImage, error)

	// StageService prepares spec.Generation, leaving it stopped. It
	// returns ErrRunning rather than touch a running Service.
	StageService(ctx context.Context, spec ServiceSpec) error

	// SetServicePower starts or stops the staged generation.
	SetServicePower(ctx context.Context, name string, power Power, generation int64) error

	// RemoveService stops and deletes the Service and its network. Its
	// state drive is kept.
	RemoveService(ctx context.Context, name string) error

	// ServiceExec runs cmd in a running Service as user ("uid:gid").
	ServiceExec(ctx context.Context, name, user string, cmd []string, stdin io.Reader) (exitCode int, output string, err error)

	// DialService opens a TCP connection to a port of a running Service
	// over its own network (the Controller is on every network).
	DialService(ctx context.Context, name string, port int) (net.Conn, error)

	// ServiceLogs streams the Service's stdout and stderr as text.
	ServiceLogs(ctx context.Context, name string, follow bool, tail int) (io.ReadCloser, error)

	// ServiceWrite writes data to path (under /run/a12s) in a running
	// Service, owned by owner ("uid:gid") with mode, in a root-owned
	// directory with dirMode: the injection path, which needs no shell in
	// the image (a12s-init does the write).
	ServiceWrite(ctx context.Context, name, path, owner, mode, dirMode string, data []byte) error

	// RefreshImage resolves ref again, pulling it when a registry has it,
	// and returns the image ID (`a12s lock update services/<svc>`).
	RefreshImage(ctx context.Context, ref string) (string, error)
}

// ServiceImage is what a Service image declares.
type ServiceImage struct {
	ImageID  string   `json:"imageId"`
	Manifest []byte   `json:"manifest,omitempty"` // /a12s/service.json, nil when absent
	Volumes  []string `json:"volumes,omitempty"`  // image VOLUMEs
	// HasCommand reports whether the image has an entrypoint or command.
	HasCommand bool `json:"hasCommand"`
}

// StateDriveID is the drive ID of a Service's state drive: "state-<svc>".
func StateDriveID(service string) string { return "state-" + service }

// StateDriveName is the name `a12s drive ls` shows: "<svc>-state".
func StateDriveName(service string) string { return service + "-state" }

// ServiceOfStateDrive returns the Service of a state drive ID.
func ServiceOfStateDrive(id string) (string, bool) { return strings.CutPrefix(id, "state-") }

// DriveState is the kind of a Service's state drive.
const DriveStateKind DriveKind = "state"

// InitDir is where the a12s-init volume is mounted read-only in a Service.
const InitDir = "/a12s/init"

// PublishSpec publishes one Service port on the host (§5.5 "publish").
type PublishSpec struct {
	IP       string `json:"ip"`
	HostPort int    `json:"hostPort"`
	Port     int    `json:"port"`
}

// ServiceSpec is everything a Driver needs to stage one Service generation.
type ServiceSpec struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
	Image      string `json:"image"` // image ID
	// User is the manifest's "uid:gid"; a12s-init drops to it.
	User string `json:"user"`
	// StateDrive and StateAt mount the state drive; empty when none.
	StateDrive string   `json:"stateDrive,omitempty"`
	StateAt    string   `json:"stateAt,omitempty"`
	Tmpfs      []string `json:"tmpfs,omitempty"`
	CPUMillis  int64    `json:"cpuMillis,omitempty"`
	Memory     int64    `json:"memoryBytes,omitempty"`
	// Env is the a12s environment (A12S_*, proxies); the image's own
	// environment is kept underneath.
	Env     []string      `json:"env,omitempty"`
	Publish []PublishSpec `json:"publish,omitempty"`
}

// Endpoint is one declared port of a used Service, as wired into the user
// (enhancements §5.4): reachable at <service>.a12s:<number> through the
// egress proxy; Env is the variable that carries its address; Tool, when
// set, is the MCP server name it is registered under.
type Endpoint struct {
	Service  string `json:"service"`
	Port     string `json:"port"`
	Number   int    `json:"number"`
	Protocol string `json:"protocol"`
	Env      string `json:"env"`
	MCPPath  string `json:"mcpPath,omitempty"`
	Tool     string `json:"tool,omitempty"`
}

// TCPEgress is a Service's named external TCP destination, reachable as
// <name>.egress.a12s:<port> through the proxy and published to the Service
// as Env=127.0.0.1:<local port> by a12s-init.
type TCPEgress struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Env  string `json:"env"`
}

// EnvName is A12S_<PREFIX>_<A>_<B> in upper case, '-' as '_'.
func EnvName(prefix string, parts ...string) string {
	out := "A12S_" + prefix
	for _, p := range parts {
		out += "_" + strings.ToUpper(strings.ReplaceAll(p, "-", "_"))
	}
	return out
}
