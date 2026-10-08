// Package docker implements driver.Driver on a Docker Engine: one container
// and one isolated internal network per Computer, named volumes for drives,
// and a read-only volume as the HarnessBundle cache (PLAN "Naming on Docker",
// "Container").
package docker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// Labels on every object a12s owns.
const (
	LabelFleet      = "a12s.fleet"
	LabelComputer   = "a12s.computer"
	LabelGeneration = "a12s.generation"
	LabelRole       = "a12s.role"
	LabelDrive      = "a12s.drive" // drive ID, on drive volumes
	LabelSize       = "a12s.size"  // requested drive size in bytes (not enforced)
	// LabelData marks the Fleet's data volume (fleet.db: inboxes, holds,
	// tokens). Like a drive, it is kept by down and destroyed only by purge.
	LabelData = "a12s.data"

	RoleController = "controller"
	RoleComputer   = "computer"
)

// DefaultSubnetPool is where per-Computer /29 networks are allocated.
var DefaultSubnetPool = netip.MustParsePrefix("10.213.0.0/16")

// SubnetBits is the prefix length of each Computer network.
const SubnetBits = 29

// StopTimeout is how long `asleep` waits before killing (§7 "bounded flush").
const StopTimeout = 25

// Capabilities kept in Computers (PLAN "Container"); everything else is dropped.
var capAdd = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "KILL"}

// Config configures a Driver.
type Config struct {
	Fleet string
	// SubnetPool defaults to DefaultSubnetPool.
	SubnetPool netip.Prefix
	// Client defaults to a client configured from the environment.
	Client *client.Client
}

// Driver is the Docker driver for one Fleet.
type Driver struct {
	cli   *client.Client
	fleet string
	pool  netip.Prefix
	mu    sync.Mutex // serializes subnet allocation and staging
}

var _ driver.Driver = (*Driver)(nil)

// New returns a Docker driver for cfg.Fleet.
func New(cfg Config) (*Driver, error) {
	if cfg.Fleet == "" {
		return nil, errors.New("docker driver: fleet name is required")
	}
	pool := cfg.SubnetPool
	if !pool.IsValid() {
		pool = DefaultSubnetPool
	}
	if !pool.Addr().Is4() || pool.Bits() > SubnetBits {
		return nil, fmt.Errorf("docker driver: subnet pool %s must be IPv4 and larger than /%d", pool, SubnetBits)
	}
	cli := cfg.Client
	if cli == nil {
		var err error
		if cli, err = client.New(client.FromEnv); err != nil {
			return nil, fmt.Errorf("docker driver: %w", err)
		}
	}
	return &Driver{cli: cli, fleet: cfg.Fleet, pool: pool.Masked()}, nil
}

// Close releases the Docker client.
func (d *Driver) Close() error { return d.cli.Close() }

// Object names (PLAN "Naming on Docker").

// ContainerName is the container, and network, of a Computer: a12s-F-C.
func (d *Driver) ContainerName(computer string) string { return "a12s-" + d.fleet + "-" + computer }

// NetworkName is the Computer's internal network: a12s-F-C.
func (d *Driver) NetworkName(computer string) string { return d.ContainerName(computer) }

// VolumeName is the volume of a drive ID: a12s-F-home-C or a12s-F-drive-D.
func (d *Driver) VolumeName(driveID string) string { return "a12s-" + d.fleet + "-" + driveID }

// BundleVolume is the read-only bundle cache: a12s-F-bundles.
func (d *Driver) BundleVolume() string { return "a12s-" + d.fleet + "-bundles" }

// ControllerName is the controller container: a12s-F-controller.
func (d *Driver) ControllerName() string { return "a12s-" + d.fleet + "-controller" }

func (d *Driver) fleetFilter() client.Filters {
	return make(client.Filters).Add("label", LabelFleet+"="+d.fleet)
}

func (d *Driver) computerFilter() client.Filters {
	return d.fleetFilter().Add("label", LabelRole+"="+RoleComputer)
}

// Capabilities reports what this engine can enforce.
func (d *Driver) Capabilities(ctx context.Context) (driver.Capabilities, error) {
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return driver.Capabilities{}, fmt.Errorf("docker info: %w", err)
	}
	sandboxed := false
	for name := range info.Info.Runtimes {
		if name == "runsc" || strings.HasPrefix(name, "kata") {
			sandboxed = true
		}
	}
	return driver.Capabilities{
		SharedWriteDrives: true,
		// gateway_mode_ipv4=isolated needs Engine 28 or later.
		EgressEnforcement: majorVersion(info.Info.ServerVersion) >= 28,
		DriveQuota:        false,
		Architectures:     []string{goArch(info.Info.Architecture)},
		SandboxedRuntime:  sandboxed,
	}, nil
}

func majorVersion(v string) int {
	major, _, _ := strings.Cut(v, ".")
	n, _ := strconv.Atoi(major)
	return n
}

func goArch(a string) string {
	switch a {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return a
}

func isNotFound(err error) bool { return cerrdefs.IsNotFound(err) }

func parseGeneration(labels map[string]string) int64 {
	g, _ := strconv.ParseInt(labels[LabelGeneration], 10, 64)
	return g
}
