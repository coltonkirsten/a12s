package driver

import (
	"context"
	"io"
	"net"
	"strings"
)

// ServiceDriver gives Services the same isolation as a Computer, with no computerd inside.
type ServiceDriver interface {
	ServiceImage(ctx context.Context, imageID string) (ServiceImage, error)

	// StageService leaves it stopped and never touches a running Service (ErrRunning).
	StageService(ctx context.Context, spec ServiceSpec) error

	SetServicePower(ctx context.Context, name string, power Power, generation int64) error

	// RemoveService keeps the state drive.
	RemoveService(ctx context.Context, name string) error

	ServiceExec(ctx context.Context, name, user string, cmd []string, stdin io.Reader) (exitCode int, output string, err error)

	// DialService works because the Controller is on every Service network.
	DialService(ctx context.Context, name string, port int) (net.Conn, error)

	ServiceLogs(ctx context.Context, name string, follow bool, tail int) (io.ReadCloser, error)

	// ServiceWrite is the injection path: a12s-init does the write, so the image needs no shell.
	ServiceWrite(ctx context.Context, name, path, owner, mode, dirMode string, data []byte) error

	LoginDriver
}

type ServiceImage struct {
	ImageID    string   `json:"imageId"`
	Manifest   []byte   `json:"manifest,omitempty"` // /a12s/service.json, nil when absent
	Volumes    []string `json:"volumes,omitempty"`  // image VOLUMEs
	HasCommand bool     `json:"hasCommand"`
}

func StateDriveID(service string) string { return "state-" + service }

func StateDriveName(service string) string { return service + "-state" }

func ServiceOfStateDrive(id string) (string, bool) { return strings.CutPrefix(id, "state-") }

const DriveStateKind DriveKind = "state"

const InitDir = "/a12s/init"

type PublishSpec struct {
	IP       string `json:"ip"`
	HostPort int    `json:"hostPort"`
	Port     int    `json:"port"`
}

type ServiceSpec struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
	Image      string `json:"image"` // image ID
	// a12s-init drops to User ("uid:gid").
	User       string   `json:"user"`
	StateDrive string   `json:"stateDrive,omitempty"`
	StateAt    string   `json:"stateAt,omitempty"`
	Tmpfs      []string `json:"tmpfs,omitempty"`
	CPUMillis  int64    `json:"cpuMillis,omitempty"`
	Memory     int64    `json:"memoryBytes,omitempty"`
	// Env is layered over the image's own environment, not replacing it.
	Env     []string      `json:"env,omitempty"`
	Publish []PublishSpec `json:"publish,omitempty"`
}

// Endpoint is reachable only at <service>.a12s:<number> through the egress proxy.
type Endpoint struct {
	Service  string `json:"service"`
	Port     string `json:"port"`
	Number   int    `json:"number"`
	Protocol string `json:"protocol"`
	Env      string `json:"env"`
	MCPPath  string `json:"mcpPath,omitempty"`
	Tool     string `json:"tool,omitempty"`
}

// TCPEgress is reached via <name>.egress.a12s:<port> through the proxy; a12s-init
// publishes it to the Service as Env=127.0.0.1:<local port>.
type TCPEgress struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Env  string `json:"env"`
}

func EnvName(prefix string, parts ...string) string {
	out := "A12S_" + prefix
	for _, p := range parts {
		out += "_" + strings.ToUpper(strings.ReplaceAll(p, "-", "_"))
	}
	return out
}

// LoginSpec is memory-only: every VOLUME on tmpfs, no state drive, on the Service's own network.
type LoginSpec struct {
	Image   string   `json:"image"`
	User    string   `json:"user"`
	Volumes []string `json:"volumes,omitempty"`
	Tmpfs   []string `json:"tmpfs,omitempty"`
	Env     []string `json:"env,omitempty"`
}

const (
	LoginDir = "/run/a12s/login"
	LoginOut = LoginDir + "/out/credential"
)

type Login struct {
	Container string   `json:"container"`
	User      string   `json:"user"`
	Read      []string `json:"read"`
}

type LoginDriver interface {
	StartLogin(ctx context.Context, service string, spec LoginSpec) (Login, error)
	RemoveLogin(ctx context.Context, service string) error
}
