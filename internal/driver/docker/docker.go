// Package docker implements driver.Driver with one container and one isolated
// internal network per Computer.
package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
)

// Labels on drive and data volumes; containers, networks and volumes also carry engine.LabelFleet.
const (
	LabelDrive = "a12s.drive" // drive ID, on drive volumes
	LabelSize  = "a12s.size"  // requested drive size in bytes (not enforced)
	LabelPath  = "a12s.path"  // a keep drive's mount point
	// LabelData's volume holds fleet.db; it survives down and only purge destroys it.
	LabelData = "a12s.data"
)

// Name selects this driver (A12S_DRIVER, driver.yaml); it is the default.
const Name = "docker"

var DefaultSubnetPool = netip.MustParsePrefix("10.213.0.0/16")

const SubnetBits = 29

type Config struct {
	Fleet      string
	SubnetPool netip.Prefix
	Client     *client.Client
	InitBinary string
}

type Driver struct {
	cli   *client.Client
	eng   *engine.Engine
	fleet string
	pool  netip.Prefix
	mu    sync.Mutex // serializes subnet allocation and staging

	callers driver.CallerMap

	initPath string
	initOnce sync.Once
	initData []byte
	initSum  string
	initErr  error
}

var _ driver.Driver = (*Driver)(nil)

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
	d := &Driver{cli: cli, eng: engine.New(cli, cfg.Fleet), fleet: cfg.Fleet, pool: pool.Masked(), initPath: cfg.InitBinary}
	d.callers.Snapshot = d.callerSnapshot
	return d, nil
}

// Open is New for the Controller running in its container: it reattaches the container to every network of the
// Fleet and reports where the owner API listens, on the Fleet bridge.
func Open(ctx context.Context, cfg Config) (*Driver, driver.Host, error) {
	d, err := New(cfg)
	if err != nil {
		return nil, driver.Host{}, err
	}
	if err := d.reattachController(ctx); err != nil {
		d.Close()
		return nil, driver.Host{}, err
	}
	addrs, err := d.controllerAddrs(ctx)
	if err != nil {
		d.Close()
		return nil, driver.Host{}, err
	}
	bridge := "a12s-" + cfg.Fleet
	ip, ok := addrs[bridge]
	if !ok {
		d.Close()
		return nil, driver.Host{}, fmt.Errorf("the controller container is not attached to network %s", bridge)
	}
	return d, driver.Host{
		ControllerName: d.ControllerName(),
		APIAddr:        net.JoinHostPort(ip.String(), strconv.Itoa(APIPort)),
	}, nil
}

// APIPort is the owner API's port on the Fleet bridge.
const APIPort = 7000

func (d *Driver) Close() error { return d.cli.Close() }

func (d *Driver) ContainerName(computer string) string { return "a12s-" + d.fleet + "-" + computer }

func (d *Driver) NetworkName(computer string) string { return d.ContainerName(computer) }

func (d *Driver) VolumeName(driveID string) string { return "a12s-" + d.fleet + "-" + driveID }

func (d *Driver) BundleVolume() string { return "a12s-" + d.fleet + "-bundles" }

func (d *Driver) ControllerName() string { return "a12s-" + d.fleet + "-controller" }

func (d *Driver) fleetFilter() client.Filters {
	return make(client.Filters).Add("label", engine.LabelFleet+"="+d.fleet)
}

func (d *Driver) computerFilter() client.Filters {
	return d.fleetFilter().Add("label", engine.LabelRole+"="+engine.RoleComputer)
}

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
		// gateway_mode_ipv4=isolated needs Engine 28 or later.
		EgressEnforcement: majorVersion(info.Info.ServerVersion) >= 28,
		DriveQuota:        false,
		Architectures:     []string{goArch(info.Info.Architecture)},
		SandboxedRuntime:  sandboxed,
		Services:          true,
		HostFolders:       true,
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
