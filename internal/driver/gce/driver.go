// Package gce implements driver.Driver on Compute Engine with one VM per Computer. The VM runs Container-Optimized
// OS and the Computer's container in its own Docker engine, with the engine package's settings; the VM is the
// isolation boundary. This package is the cloud half (VMs, disks, service accounts, firewall checks); Guest is the
// in-VM half.
package gce

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

const (
	DefaultBootImage  = "projects/cos-cloud/global/images/family/cos-stable"
	DefaultBootDiskGB = 10
	DefaultHomeDiskGB = 10
	DiskType          = "pd-balanced"

	// NetworkTag is what the fleet's firewall rules target.
	NetworkTag = "a12s-computer"
	homeDevice = "home"
)

// DefaultFamilies are tried in order when a zone has no capacity for the first; all of one architecture.
var DefaultFamilies = map[string][]string{"amd64": {"e2", "n2d", "t2d"}, "arm64": {"t2a"}}

var defaultShape = map[string]string{"amd64": "e2-small", "arm64": "t2a-standard-1"}

type Config struct {
	Fleet string
	API   *gceapi.API
	// Subnetwork is the fleet's subnet, in the API's region. Computers get no external address.
	Subnetwork string
	// Controller is the Controller VM's internal address: Computers reach its proxy and mTLS listener there.
	Controller netip.Addr
	// ControllerAccount is the Controller VM's service account, which the firewall rules name.
	ControllerAccount string
	Families          []string // default by architecture: DefaultFamilies
	// DefaultShape is used when a Computer asks for no CPU or memory.
	DefaultShape string
	BootImage    string
	BootDiskGB   int64
	// SnapshotPolicy is a resource policy attached to every home disk the driver creates; "" for none.
	SnapshotPolicy string
	// StateDir is where staged specs live, on the Controller's state disk.
	StateDir string
	// Local is the Controller VM's own engine: the Fleet's image store, as on a laptop.
	Local *engine.Engine
	Guest GuestOpener
	// Identity is IdentityNone or IdentityPerComputer.
	Identity string
	// BootMetadata is what every Computer VM boots with: the in-VM half's cloud-init and what it needs.
	BootMetadata []gceapi.MetadataItem
	// Arch is the Controller's architecture; images come from its engine, so every Computer shares it.
	Arch string
	// InternalAPI also serves the owner API on the Controller's internal address; ControllerHostname is the
	// Controller VM's internal DNS name, which its certificate names too.
	InternalAPI        bool
	ControllerHostname string
}

type Driver struct {
	cfg    Config
	api    *gceapi.API
	fleet  string
	store  specStore
	subnet netip.Prefix
	// subnetLink is the subnet's URL, as instances carry it.
	subnetLink string

	callers driver.CallerMap

	mu     sync.Mutex
	locks  map[string]*sync.Mutex // per Computer: stage, power and removal never interleave
	guests map[string]openGuest
	types  []gceapi.MachineType

	closeLocal func() error
}

type openGuest struct {
	vm VM
	g  Guest
}

var _ driver.Driver = (*Driver)(nil)

// New checks the configuration and reads the subnet; it creates nothing.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if cfg.Fleet == "" || cfg.API == nil || cfg.Subnetwork == "" || !cfg.Controller.IsValid() || cfg.Local == nil || cfg.Guest == nil || cfg.StateDir == "" {
		return nil, errors.New("gce driver: fleet, API, subnetwork, controller address, local engine, guest opener and state dir are required")
	}
	if cfg.Arch == "" {
		return nil, errors.New("gce driver: the Controller's architecture is required")
	}
	if len(cfg.Families) == 0 {
		cfg.Families = DefaultFamilies[cfg.Arch]
	}
	for _, f := range cfg.Families {
		if arch, ok := FamilyArch[f]; !ok {
			return nil, fmt.Errorf("gce driver: machine family %q is not supported (supported: e2, n2, n2d, t2d, t2a)", f)
		} else if arch != cfg.Arch {
			return nil, fmt.Errorf("gce driver: machine family %s is %s but the Controller is %s; images come from the Controller, so every family must match it", f, arch, cfg.Arch)
		}
	}
	if cfg.DefaultShape == "" {
		cfg.DefaultShape = defaultShape[cfg.Arch]
	}
	if family(cfg.DefaultShape) != cfg.Families[0] {
		return nil, fmt.Errorf("gce driver: default shape %s is not in the first family, %s", cfg.DefaultShape, cfg.Families[0])
	}
	switch cfg.Identity {
	case "":
		cfg.Identity = IdentityNone
	case IdentityNone, IdentityPerComputer:
	default:
		return nil, fmt.Errorf("gce driver: identity %q: want %s or %s", cfg.Identity, IdentityNone, IdentityPerComputer)
	}
	if cfg.BootImage == "" {
		cfg.BootImage = DefaultBootImage
	}
	if cfg.BootDiskGB == 0 {
		cfg.BootDiskGB = DefaultBootDiskGB
	}
	sn, err := cfg.API.GetSubnetwork(ctx, cfg.Subnetwork)
	if err != nil {
		return nil, fmt.Errorf("gce driver: subnetwork %s: %w", cfg.Subnetwork, err)
	}
	prefix, err := netip.ParsePrefix(sn.IPCidrRange)
	if err != nil {
		return nil, fmt.Errorf("gce driver: subnetwork %s range %q: %w", cfg.Subnetwork, sn.IPCidrRange, err)
	}
	if !prefix.Contains(cfg.Controller) {
		return nil, fmt.Errorf("gce driver: the Controller's address %s is not in subnetwork %s (%s)", cfg.Controller, cfg.Subnetwork, prefix)
	}
	d := &Driver{
		cfg: cfg, api: cfg.API, fleet: cfg.Fleet,
		store:      specStore{dir: filepath.Join(cfg.StateDir, "gce", "staged")},
		subnet:     prefix,
		subnetLink: sn.SelfLink,
		locks:      map[string]*sync.Mutex{},
		guests:     map[string]openGuest{},
	}
	if d.subnetLink == "" {
		d.subnetLink = cfg.API.RegionURL("subnetworks", cfg.Subnetwork)
	}
	d.callers.Snapshot = d.callerSnapshot
	return d, nil
}

func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, og := range d.guests {
		og.g.Close()
		delete(d.guests, name)
	}
	if d.closeLocal != nil {
		return d.closeLocal()
	}
	return nil
}

func (d *Driver) lock(computer string) func() {
	d.mu.Lock()
	l := d.locks[computer]
	if l == nil {
		l = &sync.Mutex{}
		d.locks[computer] = l
	}
	d.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (d *Driver) instanceName(computer string) string { return InstanceName(d.fleet, computer) }

func (d *Driver) diskName(driveID string) string { return DiskName(d.fleet, driveID) }

func (d *Driver) fleetFilter() string { return "labels." + LabelFleet + "=" + d.fleet }

func (d *Driver) computerFilter() string {
	return "(" + d.fleetFilter() + ") (labels." + LabelRole + "=" + RoleComputer + ")"
}

// instance returns ErrNotFound when the Computer has no VM, and refuses one not labeled for it.
func (d *Driver) instance(ctx context.Context, computer string) (*gceapi.Instance, error) {
	in, err := d.api.GetInstance(ctx, d.instanceName(computer))
	if gceapi.IsNotFound(err) {
		return nil, driver.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if in.Labels[LabelFleet] != d.fleet || in.Labels[LabelRole] != RoleComputer || in.Labels[LabelComputer] != computer {
		return nil, fmt.Errorf("instance %s exists but is not agent computer %s of fleet %s", in.Name, computer, d.fleet)
	}
	return in, nil
}

func generationOf(in *gceapi.Instance) int64 {
	g, _ := strconv.ParseInt(in.Labels[LabelGeneration], 10, 64)
	return g
}

func addressOf(in *gceapi.Instance) netip.Addr {
	if len(in.NetworkInterfaces) == 0 {
		return netip.Addr{}
	}
	a, _ := netip.ParseAddr(in.NetworkInterfaces[0].NetworkIP)
	return a
}

func vmOf(in *gceapi.Instance) VM {
	return VM{Name: in.Name, ID: in.ID, Computer: in.Labels[LabelComputer], Address: addressOf(in), Started: in.LastStartTimestamp}
}

// guest reuses an open Guest while the VM is the same boot of the same instance.
func (d *Driver) guest(ctx context.Context, in *gceapi.Instance) (Guest, error) {
	vm := vmOf(in)
	d.mu.Lock()
	og, ok := d.guests[vm.Computer]
	d.mu.Unlock()
	if ok && og.vm == vm {
		return og.g, nil
	}
	if ok {
		og.g.Close()
	}
	g, err := d.cfg.Guest(ctx, vm)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if cur, ok := d.guests[vm.Computer]; ok && cur.g != og.g {
		cur.g.Close()
	}
	d.guests[vm.Computer] = openGuest{vm: vm, g: g}
	return g, nil
}

func (d *Driver) dropGuest(computer string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if og, ok := d.guests[computer]; ok {
		og.g.Close()
		delete(d.guests, computer)
	}
}

func (d *Driver) Capabilities(ctx context.Context) (driver.Capabilities, error) {
	if err := d.checkFirewall(ctx); err != nil {
		return driver.Capabilities{}, err
	}
	return driver.Capabilities{
		EgressEnforcement: true,
		DriveQuota:        true,
		Architectures:     []string{d.cfg.Arch},
		SandboxedRuntime:  true,
		Services:          false,
		HostFolders:       false,
	}, nil
}

func (d *Driver) Caller(_, remote netip.Addr) (string, bool) { return d.callers.Lookup(remote) }

// callerSnapshot maps each running Computer VM's internal address to its Computer. VPC anti-spoofing drops any packet
// whose source isn't the sending VM's own address (the driver never enables IP forwarding), so the remote address
// names the VM.
func (d *Driver) callerSnapshot(ctx context.Context) (map[netip.Addr]string, error) {
	list, err := d.api.ListInstances(ctx, d.computerFilter())
	if err != nil {
		return nil, err
	}
	out := map[netip.Addr]string{}
	for _, in := range list {
		if in.Status != "RUNNING" {
			continue
		}
		if a := addressOf(&in); a.IsValid() {
			out[a] = in.Labels[LabelComputer]
		}
	}
	return out, nil
}

// Connect works only from an address in the fleet subnet: the firewall admits only the Controller there.
func (d *Driver) Connect(ctx context.Context, computer, service string) (net.Conn, error) {
	if service != driver.ServiceComputerd {
		return nil, fmt.Errorf("connect %s: unknown service %q", computer, service)
	}
	in, err := d.instance(ctx, computer)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", computer, err)
	}
	if in.Status != "RUNNING" {
		return nil, fmt.Errorf("connect %s: VM is %s, not running", computer, strings.ToLower(in.Status))
	}
	if on, err := onLocalNetwork(d.subnet); err != nil {
		return nil, fmt.Errorf("connect %s: %w", computer, err)
	} else if !on {
		return nil, fmt.Errorf("connect %s: %w %s; Connect must run on the Controller VM", computer, driver.ErrNotOnNetwork, d.cfg.Subnetwork)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addressOf(in).String(), strconv.Itoa(driver.ComputerdPort)))
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", computer, err)
	}
	return conn, nil
}

// onLocalNetwork is a variable so tests can run off GCE.
var onLocalNetwork = func(subnet netip.Prefix) (bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false, fmt.Errorf("list interfaces: %w", err)
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipnet.IP); ok && subnet.Contains(ip.Unmap()) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (d *Driver) runningGuest(ctx context.Context, computer string) (Guest, error) {
	in, err := d.instance(ctx, computer)
	if err != nil {
		return nil, err
	}
	if in.Status != "RUNNING" {
		return nil, fmt.Errorf("computer is asleep (VM %s)", strings.ToLower(in.Status))
	}
	return d.guest(ctx, in)
}

func (d *Driver) Exec(ctx context.Context, computer string, cmd []string, stdin io.Reader, asRoot bool) (int, string, error) {
	if len(cmd) == 0 {
		return -1, "", fmt.Errorf("exec %s: empty command", computer)
	}
	g, err := d.runningGuest(ctx, computer)
	if err != nil {
		return -1, "", fmt.Errorf("exec %s: %w", computer, err)
	}
	user := engine.AgentUser
	if asRoot {
		user = "0:0"
	}
	return g.Exec(ctx, user, cmd, stdin)
}

func (d *Driver) Terminal(ctx context.Context, computer string, rows, cols uint) (driver.Terminal, error) {
	g, err := d.runningGuest(ctx, computer)
	if err != nil {
		return nil, fmt.Errorf("terminal %s: %w", computer, err)
	}
	return g.Terminal(ctx, rows, cols)
}

func (d *Driver) Sessions(ctx context.Context, computer string) (int, error) {
	in, err := d.instance(ctx, computer)
	if errors.Is(err, driver.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if in.Status != "RUNNING" {
		return 0, nil
	}
	g, err := d.guest(ctx, in)
	if err != nil {
		return 0, err
	}
	return g.Sessions(ctx)
}

func (d *Driver) EnsureImage(ctx context.Context, ref string) (string, error) {
	id, err := d.cfg.Local.EnsureImage(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("ensure image %s: %w", ref, err)
	}
	return id, nil
}

func (d *Driver) InspectComputerImage(ctx context.Context, imageID string) (driver.ComputerImage, error) {
	return d.cfg.Local.InspectComputerImage(ctx, imageID)
}

// EnsureBundle checks the bundle on the Controller's engine; each VM extracts its own copy when it materializes.
func (d *Driver) EnsureBundle(ctx context.Context, imageRef string) (driver.Bundle, error) {
	return d.cfg.Local.EnsureBundle(ctx, imageRef, BundleVolume(d.fleet))
}

// BundleVolume is the bundle cache, on the Controller's engine and on each VM's.
func BundleVolume(fleet string) string { return "a12s-" + fleet + "-bundles" }
