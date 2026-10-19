// Package engine is what an agent computer is inside one Docker engine: its container's settings, its files, exec,
// its state and events, and the images and bundles it runs. A substrate decides where the engine runs and gives each
// container its network and mounts (Placement).
package engine

import (
	"strconv"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Container labels. Substrates put LabelFleet and LabelRole on their own objects too.
const (
	LabelFleet      = "a12s.fleet"
	LabelComputer   = "a12s.computer"
	LabelGeneration = "a12s.generation"
	LabelRole       = "a12s.role"
	LabelService    = "a12s.service"

	RoleController = "controller"
	RoleComputer   = "computer"
	RoleService    = "service"
	RoleInit       = "init"
)

// StopTimeout bounds the harness's flush on sleep before the engine kills it.
const StopTimeout = 25

const (
	HomeMount = "/home"
	agentHome = "/home/agent"
	runA12S   = "/run/a12s"
	agentUID  = 1000
	agentGID  = 1000
	AgentUser = "1000:1000"
)

// capAdd is the full set of Linux capabilities kept in Computers; everything else is dropped.
var capAdd = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "KILL"}

// NoHealthcheck replaces an image's HEALTHCHECK, which would run image code as root.
var NoHealthcheck = &container.HealthConfig{Test: []string{"NONE"}}

// Engine is one Docker engine, scoped to one Fleet.
type Engine struct {
	cli   *client.Client
	fleet string
}

func New(cli *client.Client, fleet string) *Engine { return &Engine{cli: cli, fleet: fleet} }

func (e *Engine) Client() *client.Client { return e.cli }

func (e *Engine) Fleet() string { return e.fleet }

func IsNotFound(err error) bool { return cerrdefs.IsNotFound(err) }

func ParseGeneration(labels map[string]string) int64 {
	g, _ := strconv.ParseInt(labels[LabelGeneration], 10, 64)
	return g
}
