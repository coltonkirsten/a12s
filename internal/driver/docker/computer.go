package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// Paths inside a Computer (PLAN "Container").
const (
	homeMount = "/home"
	agentHome = "/home/agent"
	runA12S   = "/run/a12s"
	agentUID  = 1000
	agentGID  = 1000
	agentUser = "1000:1000"

	// presetMarker records preset state on the home drive, outside the
	// agent's home and owned by root: "pending" while copying, "applied" after.
	presetMarker = "/home/.a12s-preset"

	gatewayModeOption = "com.docker.network.bridge.gateway_mode_ipv4"
)

// StageComputer creates the container for spec.Generation, with its network
// and mounts, leaving it stopped. A stopped older generation is replaced; the
// same generation is left as is; a running Computer is never touched.
func (d *Driver) StageComputer(ctx context.Context, spec driver.ComputerSpec) error {
	if err := checkSpec(spec); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	name := d.ContainerName(spec.Name)
	existing, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	switch {
	case isNotFound(err):
	case err != nil:
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	default:
		c := existing.Container
		if c.Config.Labels[LabelFleet] != d.fleet || c.Config.Labels[LabelRole] != RoleComputer {
			return fmt.Errorf("stage %s: container %s exists but is not a Computer of fleet %s", spec.Name, name, d.fleet)
		}
		if c.State != nil && (c.State.Running || c.State.Restarting || c.State.Paused) {
			return fmt.Errorf("stage %s generation %d: %w", spec.Name, spec.Generation, driver.ErrRunning)
		}
		staged := parseGeneration(c.Config.Labels)
		if staged > spec.Generation {
			return fmt.Errorf("stage %s generation %d: generation %d is staged: %w", spec.Name, spec.Generation, staged, driver.ErrStaleGeneration)
		}
		if staged == spec.Generation {
			// Same generation: done only if its bootstrap finished; a stage
			// interrupted mid-way is replaced, never reported as success.
			done, err := d.bootstrapped(ctx, c.ID, spec)
			if err != nil {
				return fmt.Errorf("stage %s: %w", spec.Name, err)
			}
			if done {
				return nil
			}
		}
		if _, err := d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{}); err != nil {
			return fmt.Errorf("stage %s: remove generation %s: %w", spec.Name, c.Config.Labels[LabelGeneration], err)
		}
	}

	if _, err := d.ensureImage(ctx, spec.Image); err != nil {
		return fmt.Errorf("stage %s: image %s: %w", spec.Name, spec.Image, err)
	}
	if _, err := d.ensureBundleVolume(ctx); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	netName, err := d.ensureNetwork(ctx, spec.Name)
	if err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}
	if err := d.ConnectController(ctx, netName); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, err)
	}

	created, err := d.cli.ContainerCreate(ctx, d.createOptions(spec))
	if err != nil {
		return fmt.Errorf("stage %s: create container: %w", spec.Name, err)
	}
	if err := d.checkBundle(ctx, created.ID, spec); err != nil {
		return fmt.Errorf("stage %s: %w", spec.Name, d.discard(ctx, created.ID, err))
	}
	if len(spec.Preset) > 0 {
		if err := d.applyPreset(ctx, created.ID, spec.Preset); err != nil {
			// Without the container, a retry stages (and copies) again.
			return fmt.Errorf("stage %s: %w", spec.Name, d.discard(ctx, created.ID, err))
		}
	}
	return nil
}

// discard removes a half-staged container after cause, and reports a failed
// removal alongside cause rather than assuming the container is gone.
func (d *Driver) discard(ctx context.Context, containerID string, cause error) error {
	if _, err := d.cli.ContainerRemove(context.WithoutCancel(ctx), containerID, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
		return errors.Join(cause, fmt.Errorf("remove half-staged container: %w", err))
	}
	return cause
}

// bootstrapped reports whether an existing container of spec's generation
// finished staging: its bundle is in the cache and, with a preset, the
// marker says "applied" (or the home was initialized without one).
func (d *Driver) bootstrapped(ctx context.Context, containerID string, spec driver.ComputerSpec) (bool, error) {
	if err := d.checkBundle(ctx, containerID, spec); err != nil {
		return false, nil
	}
	if len(spec.Preset) == 0 {
		return true, nil
	}
	state, err := d.readFile(ctx, containerID, presetMarker)
	if err != nil {
		return false, fmt.Errorf("preset: %w", err)
	}
	switch state {
	case "applied":
		return true, nil
	case "": // no marker: finished only if the home was already initialized
		_, err := d.cli.ContainerStatPath(ctx, containerID, client.ContainerStatPathOptions{Path: agentHome})
		if isNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("preset: stat %s: %w", agentHome, err)
		}
		return true, nil
	}
	return false, nil // "pending": the copy did not finish
}

// mountPointRE is the character set a mount point may use; A12S_DRIVES
// separates entries with ',' and modes with ':'.
var mountPointRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// reservedMount reports whether a work drive would mount over a path the
// Computer itself owns.
func reservedMount(at string) bool {
	for _, r := range []string{"/", homeMount, "/run", "/opt/a12s", "/tmp", "/proc", "/sys", "/dev"} {
		if at == r || (r != "/" && strings.HasPrefix(at, r+"/")) {
			return true
		}
	}
	return false
}

func checkSpec(s driver.ComputerSpec) error {
	switch {
	case s.Name == "":
		return errors.New("name is required")
	case s.Generation < 1:
		return errors.New("generation must be positive")
	case s.Image == "":
		return errors.New("image is required")
	case s.HomeDrive == "":
		return errors.New("home drive is required")
	case s.Bundle.Kind == "" || s.Bundle.Version == "":
		return errors.New("bundle kind and version are required")
	case driver.ShortImageID(s.Bundle.Image) == "":
		return fmt.Errorf("bundle image %q must be the image ID EnsureBundle returned", s.Bundle.Image)
	}
	for _, m := range s.Drives {
		if !mountPointRE.MatchString(m.At) || path.Clean(m.At) != m.At || !path.IsAbs(m.At) || reservedMount(m.At) {
			return fmt.Errorf("drive %s: invalid mount point %q", m.DriveID, m.At)
		}
	}
	return nil
}

// createOptions builds the container for one Computer generation.
func (d *Driver) createOptions(spec driver.ComputerSpec) client.ContainerCreateOptions {
	if spec.ProxyURL == "" {
		spec.ProxyURL = "http://" + d.ControllerName() + ":3128"
	}
	if spec.ControllerURL == "" {
		spec.ControllerURL = "https://" + d.ControllerName() + ":7443"
	}
	netName := d.NetworkName(spec.Name)
	volume := func(source, target string, readOnly bool) mount.Mount {
		return mount.Mount{
			Type: mount.TypeVolume, Source: source, Target: target, ReadOnly: readOnly,
			// A drive holds only what was written to it, never image content.
			VolumeOptions: &mount.VolumeOptions{NoCopy: true},
		}
	}
	mounts := []mount.Mount{
		volume(d.VolumeName(spec.HomeDrive), homeMount, false),
		volume(d.BundleVolume(), driver.BundleRoot, true),
	}
	for _, m := range spec.Drives {
		mounts = append(mounts, volume(d.VolumeName(m.DriveID), m.At, m.ReadOnly))
	}
	security := []string{"no-new-privileges:true"}
	if spec.Seccomp != "" {
		security = append(security, "seccomp="+spec.Seccomp)
	}
	stop := StopTimeout
	useInit := true
	return client.ContainerCreateOptions{
		Name: d.ContainerName(spec.Name),
		Config: &container.Config{
			Image:       spec.Image,
			Hostname:    spec.Name,
			Env:         spec.Env(d.fleet),
			StopTimeout: &stop,
			Labels: map[string]string{
				LabelFleet:      d.fleet,
				LabelComputer:   spec.Name,
				LabelGeneration: strconv.FormatInt(spec.Generation, 10),
				LabelRole:       RoleComputer,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(netName),
			RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
			Init:           &useInit, // --init: reap orphans, forward signals to computerd
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			CapAdd:         capAdd,
			SecurityOpt:    security,
			Tmpfs: map[string]string{
				runA12S: "rw,nosuid,nodev,noexec,mode=0700,size=16m",
				"/tmp":  "rw,nosuid,nodev,mode=1777",
			},
			Mounts: mounts,
			Resources: container.Resources{
				Memory:   spec.Memory,
				NanoCPUs: spec.CPUMillis * 1_000_000,
			},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
		},
	}
}

// checkBundle verifies the Computer's bundle is in the cache.
func (d *Driver) checkBundle(ctx context.Context, containerID string, spec driver.ComputerSpec) error {
	p := path.Join(spec.Bundle.Path(), bundleManifest)
	_, err := d.cli.ContainerStatPath(ctx, containerID, client.ContainerStatPathOptions{Path: p})
	if isNotFound(err) {
		return fmt.Errorf("bundle %s@%s (%s) is not in the cache; EnsureBundle first", spec.Bundle.Kind, spec.Bundle.Version, spec.Bundle.Image)
	}
	if err != nil {
		return fmt.Errorf("check bundle: %w", err)
	}
	return nil
}

// applyPreset copies a preset tar into /home/agent of a stopped container,
// owned by the agent, once per home drive: when the home is still empty
// (before computerd first creates /home/agent), or when an earlier copy did
// not finish. A root-owned marker beside /home/agent reads "pending" during
// the copy and "applied" only after it succeeded.
func (d *Driver) applyPreset(ctx context.Context, containerID string, preset []byte) error {
	state, err := d.readFile(ctx, containerID, presetMarker)
	if err != nil {
		return fmt.Errorf("preset: %w", err)
	}
	switch state {
	case "applied":
		return nil
	case "": // no marker: apply only onto an empty home
		_, err := d.cli.ContainerStatPath(ctx, containerID, client.ContainerStatPathOptions{Path: agentHome})
		if err == nil {
			return nil // home initialized without a preset
		}
		if !isNotFound(err) {
			return fmt.Errorf("preset: stat %s: %w", agentHome, err)
		}
	}
	if err := d.writeMarker(ctx, containerID, "pending"); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := rebasePresetTar(bytes.NewReader(preset), &buf, path.Base(agentHome)); err != nil {
		return err
	}
	if _, err := d.cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: homeMount, Content: &buf}); err != nil {
		return fmt.Errorf("preset: copy: %w", err)
	}
	return d.writeMarker(ctx, containerID, "applied")
}

func (d *Driver) writeMarker(ctx context.Context, containerID, state string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: path.Base(presetMarker), Mode: 0o600, Size: int64(len(state))})
	_, _ = tw.Write([]byte(state))
	_ = tw.Close()
	if _, err := d.cli.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: path.Dir(presetMarker), Content: &buf}); err != nil {
		return fmt.Errorf("preset: write marker: %w", err)
	}
	return nil
}

// readFile returns a small file's content from a container, or "" if it
// does not exist.
func (d *Driver) readFile(ctx context.Context, containerID, p string) (string, error) {
	res, err := d.cli.CopyFromContainer(ctx, containerID, client.CopyFromContainerOptions{SourcePath: p})
	if isNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	defer res.Content.Close()
	tr := tar.NewReader(res.Content)
	if _, err := tr.Next(); err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	b, err := io.ReadAll(io.LimitReader(tr, 4096))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	return string(b), nil
}

// rebasePresetTar prefixes every entry with dir/, adds dir/ itself, and makes
// the agent own everything. Entries that would leave dir are rejected.
func rebasePresetTar(r io.Reader, w io.Writer, dir string) error {
	tr, tw := tar.NewReader(r), tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755, Uid: agentUID, Gid: agentGID}); err != nil {
		return fmt.Errorf("preset tar: %w", err)
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("preset tar: %w", err)
		}
		clean := path.Clean("/" + h.Name)
		if clean == "/" {
			continue
		}
		if h.Typeflag == tar.TypeLink {
			return fmt.Errorf("preset tar: hard link %q not supported", h.Name)
		}
		h.Name = dir + clean
		if h.Typeflag == tar.TypeDir {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = agentUID, agentGID, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("preset tar: %w", err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return fmt.Errorf("preset tar: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("preset tar: %w", err)
	}
	return nil
}

// ensureNetwork creates the Computer's isolated internal network with a /29
// from the pool, unless it already exists.
func (d *Driver) ensureNetwork(ctx context.Context, computer string) (string, error) {
	return d.ensureNetworkFor(ctx, computer, RoleComputer)
}

// ownerLabel is the label naming a network's or container's owner by role.
func ownerLabel(role string) string {
	if role == RoleService {
		return LabelService
	}
	return LabelComputer
}

// ensureNetworkFor creates the isolated internal network of a Computer or
// a Service (role), unless it already exists.
func (d *Driver) ensureNetworkFor(ctx context.Context, computer, role string) (string, error) {
	name := d.NetworkName(computer)
	res, err := d.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		if err := d.checkNetwork(res.Network.Network, computer, role); err != nil {
			return "", err
		}
		return name, nil
	}
	if !isNotFound(err) {
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
			LabelFleet:       d.fleet,
			ownerLabel(role): computer,
			LabelRole:        role,
		},
	})
	if err != nil {
		return "", fmt.Errorf("create network %s: %w", name, err)
	}
	return name, nil
}

// checkNetwork accepts an existing network for reuse only if it is exactly
// what ensureNetwork would create: internal, isolated gateway, our labels.
func (d *Driver) checkNetwork(n network.Network, computer, role string) error {
	switch {
	case !n.Internal:
		return fmt.Errorf("network %s exists but is not internal; remove it", n.Name)
	case n.Options[gatewayModeOption] != "isolated":
		return fmt.Errorf("network %s exists without %s=isolated; remove it", n.Name, gatewayModeOption)
	case n.Labels[LabelFleet] != d.fleet || n.Labels[ownerLabel(role)] != computer || n.Labels[LabelRole] != role:
		return fmt.Errorf("network %s exists but is not the network of %s %s in fleet %s", n.Name, role, computer, d.fleet)
	}
	return nil
}

// allocateSubnet returns the first /bits prefix in pool that overlaps none of used.
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

// ConnectController attaches the Fleet's controller container to a Computer
// network, so it can reach computerd and serve as that network's proxy. It
// does nothing when there is no controller container (yet).
func (d *Driver) ConnectController(ctx context.Context, networkName string) error {
	res, err := d.cli.ContainerInspect(ctx, d.ControllerName(), client.ContainerInspectOptions{})
	if isNotFound(err) {
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

// SetPower starts or stops the staged generation.
func (d *Driver) SetPower(ctx context.Context, computer string, power driver.Power, generation int64) error {
	res, err := d.cli.ContainerInspect(ctx, d.ContainerName(computer), client.ContainerInspectOptions{})
	if isNotFound(err) {
		return fmt.Errorf("power %s %s: %w", computer, power, driver.ErrNotStaged)
	}
	if err != nil {
		return fmt.Errorf("power %s %s: %w", computer, power, err)
	}
	c := res.Container
	switch staged := parseGeneration(c.Config.Labels); {
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
		if _, err := d.cli.ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("power %s awake: %w", computer, err)
		}
	case driver.PowerAsleep:
		if !running {
			return nil
		}
		timeout := StopTimeout
		if _, err := d.cli.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
			return fmt.Errorf("power %s asleep: %w", computer, err)
		}
	default:
		return fmt.Errorf("power %s: unknown state %q", computer, power)
	}
	return nil
}

// RemoveComputer deletes the Computer's container and network. Its drives
// are kept. Removing a missing Computer succeeds.
func (d *Driver) RemoveComputer(ctx context.Context, computer string) error {
	name := d.ContainerName(computer)
	res, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	switch {
	case isNotFound(err):
	case err != nil:
		return fmt.Errorf("remove %s: %w", computer, err)
	case res.Container.Config.Labels[LabelFleet] != d.fleet || res.Container.Config.Labels[LabelRole] != RoleComputer:
		return fmt.Errorf("remove %s: container %s is not a Computer of fleet %s", computer, name, d.fleet)
	default:
		if _, err := d.cli.ContainerRemove(ctx, res.Container.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
			return fmt.Errorf("remove %s: %w", computer, err)
		}
	}

	netName := d.NetworkName(computer)
	nres, err := d.cli.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove %s: %w", computer, err)
	}
	if nres.Network.Labels[LabelFleet] != d.fleet {
		return fmt.Errorf("remove %s: network %s is not owned by fleet %s", computer, netName, d.fleet)
	}
	for id := range nres.Network.Containers {
		_, _ = d.cli.NetworkDisconnect(ctx, netName, client.NetworkDisconnectOptions{Container: id, Force: true})
	}
	if _, err := d.cli.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove %s: network: %w", computer, err)
	}
	return nil
}

// ControllerAddrs returns the controller container's IP on each network it
// is attached to, by network name. The Controller uses it to find its own
// address on the Fleet bridge (where the API listens) and on each Computer
// network (which identifies the Computer behind an egress proxy connection).
func (d *Driver) ControllerAddrs(ctx context.Context) (map[string]netip.Addr, error) {
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
