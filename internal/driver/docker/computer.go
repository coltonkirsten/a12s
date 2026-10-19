package docker

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
)

const gatewayModeOption = "com.docker.network.bridge.gateway_mode_ipv4"

func (d *Driver) StageComputer(ctx context.Context, spec driver.ComputerSpec) error {
	if err := engine.CheckSpec(spec); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	name := d.ContainerName(spec.Name)
	existing, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	switch {
	case engine.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	default:
		c := existing.Container
		if c.Config.Labels[engine.LabelFleet] != d.fleet || c.Config.Labels[engine.LabelRole] != engine.RoleComputer {
			return fmt.Errorf("stage %s: container %s exists but is not an agent computer of fleet %s", spec.Name, name, d.fleet)
		}
		if c.State != nil && (c.State.Running || c.State.Restarting || c.State.Paused) {
			return fmt.Errorf("stage %s generation %d: %w", spec.Name, spec.Generation, driver.ErrRunning)
		}
		staged := engine.ParseGeneration(c.Config.Labels)
		if staged > spec.Generation {
			return fmt.Errorf("stage %s generation %d: generation %d is staged: %w", spec.Name, spec.Generation, staged, driver.ErrStaleGeneration)
		}
		if staged == spec.Generation {
			// A stage interrupted mid-way is replaced, never reported as success.
			done, err := d.eng.Bootstrapped(ctx, c.ID, spec)
			if err != nil {
				return fmt.Errorf("stage %s: %w", spec.Name, err)
			}
			if done {
				return nil
			}
		}
		if _, err := d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{}); err != nil {
			return fmt.Errorf("stage %s: remove generation %s: %w", spec.Name, c.Config.Labels[engine.LabelGeneration], err)
		}
	}

	if _, err := d.eng.EnsureImage(ctx, spec.Image); err != nil {
		return fmt.Errorf("stage %s: image %s: %w", spec.Name, spec.Image, err)
	}
	if _, err := d.eng.EnsureVolume(ctx, d.BundleVolume(), map[string]string{engine.LabelFleet: d.fleet}, engine.LabelFleet, d.fleet); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	netName, err := d.ensureNetwork(ctx, spec.Name)
	if err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	if err := d.ConnectController(ctx, netName); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}

	if _, err := d.eng.CreateComputer(ctx, spec, d.placement(spec.Name)); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	return nil
}

func (d *Driver) placement(computer string) engine.Placement {
	return engine.Placement{
		Name:    d.ContainerName(computer),
		Network: d.NetworkName(computer),
		Drive: func(driveID, target string) mount.Mount {
			return engine.VolumeMount(d.VolumeName(driveID), target, false)
		},
		BundleVolume:  d.BundleVolume(),
		ProxyURL:      "http://" + d.ControllerName() + ":3128",
		ControllerURL: "https://" + d.ControllerName() + ":7443",
		Limits:        true,
	}
}

func (d *Driver) createOptions(spec driver.ComputerSpec) client.ContainerCreateOptions {
	return d.eng.CreateOptions(spec, d.placement(spec.Name))
}

func (d *Driver) ensureNetwork(ctx context.Context, computer string) (string, error) {
	return d.ensureNetworkFor(ctx, computer, engine.RoleComputer)
}

func ownerLabel(role string) string {
	if role == engine.RoleService {
		return engine.LabelService
	}
	return engine.LabelComputer
}

func (d *Driver) ensureNetworkFor(ctx context.Context, computer, role string) (string, error) {
	name := d.NetworkName(computer)
	res, err := d.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		if err := d.checkNetwork(res.Network.Network, computer, role); err != nil {
			return "", err
		}
		return name, nil
	}
	if !engine.IsNotFound(err) {
		return "", fmt.Errorf("inspect network %s: %w", name, err)
	}
	list, err := d.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}
	var used []netip.Prefix
	for _, n := range list.Items {
		for _, c := range n.IPAM.Config {
			if c.Subnet.IsValid() {
				used = append(used, c.Subnet)
			}
		}
	}
	subnet, err := allocateSubnet(d.pool, SubnetBits, used)
	if err != nil {
		return "", err
	}
	_, err = d.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: true,
		IPAM:     &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet}}},
		Options:  map[string]string{gatewayModeOption: "isolated"},
		Labels: map[string]string{
			engine.LabelFleet: d.fleet,
			ownerLabel(role):  computer,
			engine.LabelRole:  role,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return name, nil
}

// checkNetwork refuses to reuse a network that isn't internal, gateway-isolated and ours.
func (d *Driver) checkNetwork(n network.Network, computer, role string) error {
	switch {
	case !n.Internal:
		return fmt.Errorf("network %s exists but is not internal; remove it", n.Name)
	case n.Options[gatewayModeOption] != "isolated":
		return fmt.Errorf("network %s exists without %s=isolated; remove it", n.Name, gatewayModeOption)
	case n.Labels[engine.LabelFleet] != d.fleet || n.Labels[ownerLabel(role)] != computer || n.Labels[engine.LabelRole] != role:
		return fmt.Errorf("network %s exists but is not the network of %s %s in fleet %s", n.Name, role, computer, d.fleet)
	}
	return nil
}

func allocateSubnet(pool netip.Prefix, bits int, used []netip.Prefix) (netip.Prefix, error) {
	size := uint32(1) << (32 - bits)
	start := pool.Masked().Addr().As4()
	base := uint32(start[0])<<24 | uint32(start[1])<<16 | uint32(start[2])<<8 | uint32(start[3])
	count := uint32(1) << (bits - pool.Bits())
next:
	for i := uint32(0); i < count; i++ {
		a := base + i*size
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a)}), bits)
		for _, u := range used {
			if p.Overlaps(u) {
				continue next
			}
		}
		return p, nil
	}
	return netip.Prefix{}, fmt.Errorf("subnet pool %s is exhausted", pool)
}

// reattachController connects the controller container to every agent computer's and Service's network: a
// Controller container that was replaced (an upgrade) starts attached to the Fleet bridge only.
func (d *Driver) reattachController(ctx context.Context) error {
	f := make(client.Filters).Add("label", engine.LabelFleet+"="+d.fleet)
	list, err := d.cli.NetworkList(ctx, client.NetworkListOptions{Filters: f})
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	for _, n := range list.Items {
		if role := n.Labels[engine.LabelRole]; role != engine.RoleComputer && role != engine.RoleService {
			continue
		}
		if err := d.ConnectController(ctx, n.Name); err != nil {
			return err
		}
	}
	return nil
}

// ConnectController is a no-op when no controller container exists yet.
func (d *Driver) ConnectController(ctx context.Context, networkName string) error {
	res, err := d.cli.ContainerInspect(ctx, d.ControllerName(), client.ContainerInspectOptions{})
	if engine.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect controller: %w", err)
	}
	if res.Container.NetworkSettings != nil {
		if _, ok := res.Container.NetworkSettings.Networks[networkName]; ok {
			return nil
		}
	}
	_, err = d.cli.NetworkConnect(ctx, networkName, client.NetworkConnectOptions{Container: res.Container.ID})
	if err != nil {
		return fmt.Errorf("connect controller to %s: %w", networkName, err)
	}
	return nil
}

func (d *Driver) SetPower(ctx context.Context, computer string, power driver.Power, generation int64) error {
	res, err := d.cli.ContainerInspect(ctx, d.ContainerName(computer), client.ContainerInspectOptions{})
	if engine.IsNotFound(err) {
		return fmt.Errorf("power %s %s: %w", computer, power, driver.ErrNotStaged)
	}
	if err != nil {
		return fmt.Errorf("power %s %s: %w", computer, power, err)
	}
	c := res.Container
	switch staged := engine.ParseGeneration(c.Config.Labels); {
	case generation < staged:
		return fmt.Errorf("power %s generation %d: generation %d is staged: %w", computer, generation, staged, driver.ErrStaleGeneration)
	case generation > staged:
		return fmt.Errorf("power %s generation %d: generation %d is staged: %w", computer, generation, staged, driver.ErrNotStaged)
	}
	running := c.State != nil && (c.State.Running || c.State.Restarting || c.State.Paused)
	switch power {
	case driver.PowerAwake:
		if running {
			return nil
		}
		if err := d.callers.Require(ctx, computer); err != nil {
			return fmt.Errorf("power %s awake: %w", computer, err)
		}
		if _, err := d.cli.ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("power %s awake: %w", computer, engine.ExplainMountError(err, engine.BoundFolders(c)))
		}
	case driver.PowerAsleep:
		if !running {
			return nil
		}
		timeout := engine.StopTimeout
		if _, err := d.cli.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
			return fmt.Errorf("power %s asleep: %w", computer, err)
		}
	default:
		return fmt.Errorf("power %s: unknown state %q", computer, power)
	}
	return nil
}

func (d *Driver) RemoveComputer(ctx context.Context, computer string) error {
	defer d.callers.Remove(computer)()
	name := d.ContainerName(computer)
	res, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	switch {
	case engine.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("remove %s: %w", computer, err)
	case res.Container.Config.Labels[engine.LabelFleet] != d.fleet || res.Container.Config.Labels[engine.LabelRole] != engine.RoleComputer:
		return fmt.Errorf("remove %s: container %s is not an agent computer of fleet %s", computer, name, d.fleet)
	default:
		if _, err := d.cli.ContainerRemove(ctx, res.Container.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !engine.IsNotFound(err) {
			return fmt.Errorf("remove %s: %w", computer, err)
		}
	}

	netName := d.NetworkName(computer)
	nres, err := d.cli.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
	if engine.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove %s: %w", computer, err)
	}
	if nres.Network.Labels[engine.LabelFleet] != d.fleet {
		return fmt.Errorf("remove %s: network %s is not owned by fleet %s", computer, netName, d.fleet)
	}
	for id := range nres.Network.Containers {
		_, _ = d.cli.NetworkDisconnect(ctx, netName, client.NetworkDisconnectOptions{Container: id, Force: true})
	}
	if _, err := d.cli.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{}); err != nil && !engine.IsNotFound(err) {
		return fmt.Errorf("remove %s: network: %w", computer, err)
	}
	return nil
}

// controllerAddrs is the controller container's address on each network, by network name.
func (d *Driver) controllerAddrs(ctx context.Context) (map[string]netip.Addr, error) {
	res, err := d.cli.ContainerInspect(ctx, d.ControllerName(), client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect controller: %w", err)
	}
	out := map[string]netip.Addr{}
	if res.Container.NetworkSettings != nil {
		for name, ep := range res.Container.NetworkSettings.Networks {
			if ep != nil && ep.IPAddress.IsValid() {
				out[name] = ep.IPAddress.Unmap()
			}
		}
	}
	return out, nil
}

// callerSnapshot maps the Controller's address on each Computer's and Service's own network to its owner: only the
// Controller and that one Computer are on the network, so a connection arriving on that address is from it.
func (d *Driver) callerSnapshot(ctx context.Context) (map[netip.Addr]string, error) {
	addrs, err := d.controllerAddrs(ctx)
	if err != nil {
		return nil, err
	}
	list, err := d.cli.NetworkList(ctx, client.NetworkListOptions{Filters: d.fleetFilter()})
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	out := map[netip.Addr]string{}
	for _, n := range list.Items {
		role := n.Labels[engine.LabelRole]
		if role != engine.RoleComputer && role != engine.RoleService {
			continue
		}
		owner := n.Labels[ownerLabel(role)]
		if ip, ok := addrs[n.Name]; ok && owner != "" && n.Name == d.NetworkName(owner) {
			out[ip] = owner
		}
	}
	return out, nil
}

func (d *Driver) Caller(local, _ netip.Addr) (string, bool) { return d.callers.Lookup(local) }
