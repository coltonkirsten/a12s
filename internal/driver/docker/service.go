package docker

import (
	"archive/tar"
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// serviceCapAdd is needed only for a12s-init's bootstrap; it drops all of them
// when it switches to the manifest uid.
var serviceCapAdd = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID"}

const initCacheMount = "/a12s-init-cache"

var noHealthcheck = &container.HealthConfig{Test: []string{"NONE"}}

func (d *Driver) InitVolume() string { return "a12s-" + d.fleet + "-init" }

func (d *Driver) serviceFilter() client.Filters {
	return d.fleetFilter().Add("label", LabelRole+"="+RoleService)
}

func (d *Driver) initBinary() ([]byte, string, error) {
	d.initOnce.Do(func() {
		p := d.initPath
		if p == "" {
			var err error
			if p, err = os.Executable(); err != nil {
				d.initErr = err
				return
			}
		}
		data, err := os.ReadFile(p)
		if err != nil {
			d.initErr = fmt.Errorf("read a12s-init binary: %w", err)
			return
		}
		sum := sha256.Sum256(data)
		d.initData, d.initSum = data, hex.EncodeToString(sum[:])[:12]
	})
	return d.initData, d.initSum, d.initErr
}

func (d *Driver) initExec() (string, error) {
	_, sum, err := d.initBinary()
	if err != nil {
		return "", err
	}
	return path.Join(driver.InitDir, sum, "a12s"), nil
}

// ensureInit uses a never-started container, so the Service image needs no shell.
func (d *Driver) ensureInit(ctx context.Context, imageID string) error {
	data, sum, err := d.initBinary()
	if err != nil {
		return err
	}
	if _, err := d.ensureVolume(ctx, d.InitVolume(), map[string]string{LabelFleet: d.fleet, LabelRole: RoleInit}, LabelRole, RoleInit); err != nil {
		return err
	}
	suffix := make([]byte, 4)
	_, _ = crand.Read(suffix)
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "a12s-" + d.fleet + "-init-extract-" + hex.EncodeToString(suffix),
		Config: &container.Config{
			Image: imageID, Entrypoint: []string{"/a12s-never-started"},
			Labels: map[string]string{LabelFleet: d.fleet},
		},
		HostConfig: &container.HostConfig{NetworkMode: "none", Mounts: []mount.Mount{{
			Type: mount.TypeVolume, Source: d.InitVolume(), Target: initCacheMount,
			VolumeOptions: &mount.VolumeOptions{NoCopy: true},
		}}},
	})
	if err != nil {
		return fmt.Errorf("a12s-init: create extractor: %w", err)
	}
	defer d.removeExtractor(ctx, created.ID)
	target := path.Join(initCacheMount, sum, "a12s")
	if st, err := d.cli.ContainerStatPath(ctx, created.ID, client.ContainerStatPathOptions{Path: target}); err == nil && st.Stat.Size == int64(len(data)) {
		return nil
	} else if err != nil && !isNotFound(err) {
		return fmt.Errorf("a12s-init: stat: %w", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: sum + "/", Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: sum + "/a12s", Mode: 0o755, Size: int64(len(data))})
	_, _ = tw.Write(data)
	_ = tw.Close()
	if _, err := d.cli.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: initCacheMount, Content: &buf}); err != nil {
		return fmt.Errorf("a12s-init: copy: %w", err)
	}
	return nil
}

func (d *Driver) ServiceImage(ctx context.Context, imageID string) (driver.ServiceImage, error) {
	res, err := d.cli.ImageInspect(ctx, imageID)
	if err != nil {
		return driver.ServiceImage{}, fmt.Errorf("inspect image %s: %w", imageID, err)
	}
	out := driver.ServiceImage{ImageID: res.ID}
	if cfg := res.Config; cfg != nil {
		for v := range cfg.Volumes {
			out.Volumes = append(out.Volumes, v)
		}
		out.HasCommand = len(cfg.Entrypoint) > 0 || len(cfg.Cmd) > 0
	}
	sort.Strings(out.Volumes)
	id, err := d.createExtractor(ctx, res.ID, false)
	if err != nil {
		return out, fmt.Errorf("read %s from %s: %w", fleet.ManifestPath, imageID, err)
	}
	defer d.removeExtractor(ctx, id)
	rc, err := d.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: fleet.ManifestPath})
	if isNotFound(err) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("read %s: %w", fleet.ManifestPath, err)
	}
	defer rc.Content.Close()
	tr := tar.NewReader(rc.Content)
	h, err := tr.Next()
	if err != nil {
		return out, fmt.Errorf("read %s: %w", fleet.ManifestPath, err)
	}
	if h.Typeflag != tar.TypeReg {
		return out, fmt.Errorf("%s is not a regular file", fleet.ManifestPath)
	}
	if out.Manifest, err = io.ReadAll(io.LimitReader(tr, 1<<20)); err != nil {
		return out, fmt.Errorf("read %s: %w", fleet.ManifestPath, err)
	}
	return out, nil
}

func (d *Driver) StageService(ctx context.Context, spec driver.ServiceSpec) error {
	if err := checkServiceSpec(spec); err != nil {
		return fmt.Errorf("stage service %s: %w", spec.Name, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	name := d.ContainerName(spec.Name)
	existing, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	switch {
	case isNotFound(err):
	case err != nil:
		return fmt.Errorf("stage service %s: %w", spec.Name, err)
	default:
		c := existing.Container
		if c.Config.Labels[LabelFleet] != d.fleet || c.Config.Labels[LabelRole] != RoleService {
			return fmt.Errorf("stage service %s: container %s exists but is not a Service of fleet %s", spec.Name, name, d.fleet)
		}
		if c.State != nil && (c.State.Running || c.State.Restarting || c.State.Paused) {
			return fmt.Errorf("stage service %s generation %d: %w", spec.Name, spec.Generation, driver.ErrRunning)
		}
		staged := parseGeneration(c.Config.Labels)
		if staged > spec.Generation {
			return fmt.Errorf("stage service %s generation %d: generation %d is staged: %w", spec.Name, spec.Generation, staged, driver.ErrStaleGeneration)
		}
		if staged == spec.Generation {
			return nil
		}
		if _, err := d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{}); err != nil {
			return fmt.Errorf("stage service %s: remove generation %d: %w", spec.Name, staged, err)
		}
	}
	img, err := d.cli.ImageInspect(ctx, spec.Image)
	if err != nil {
		return fmt.Errorf("stage service %s: image %s: %w", spec.Name, spec.Image, err)
	}
	var program []string
	if img.Config != nil {
		program = append(append(program, img.Config.Entrypoint...), img.Config.Cmd...)
	}
	if len(program) == 0 {
		return fmt.Errorf("stage service %s: image %s has no entrypoint or command", spec.Name, spec.Image)
	}
	if err := d.ensureInit(ctx, img.ID); err != nil {
		return fmt.Errorf("stage service %s: %w", spec.Name, err)
	}
	initExec, err := d.initExec()
	if err != nil {
		return fmt.Errorf("stage service %s: %w", spec.Name, err)
	}
	netName, err := d.ensureNetworkFor(ctx, spec.Name, RoleService)
	if err != nil {
		return fmt.Errorf("stage service %s: %w", spec.Name, err)
	}
	if err := d.ConnectController(ctx, netName); err != nil {
		return fmt.Errorf("stage service %s: %w", spec.Name, err)
	}
	// Docker gives every unmounted image VOLUME an anonymous disk volume, even
	// under a tmpfs; mount each as tmpfs so nothing reaches disk.
	if img.Config != nil {
		for v := range img.Config.Volumes {
			v = path.Clean(v)
			if v == spec.StateAt || slices.Contains(spec.Tmpfs, v) {
				continue
			}
			spec.Tmpfs = append(spec.Tmpfs, v)
		}
		sort.Strings(spec.Tmpfs)
	}
	opts := d.serviceCreateOptions(spec, initExec, program)
	if _, err := d.cli.ContainerCreate(ctx, opts); err != nil {
		return fmt.Errorf("stage service %s: create container: %w", spec.Name, err)
	}
	return nil
}

func checkServiceSpec(s driver.ServiceSpec) error {
	switch {
	case s.Name == "":
		return errors.New("name is required")
	case s.Generation < 1:
		return errors.New("generation must be positive")
	case s.Image == "":
		return errors.New("image is required")
	case s.User == "" || strings.HasPrefix(s.User, "0:") || s.User == "0":
		return fmt.Errorf("user %q: a Service never runs as root", s.User)
	case (s.StateDrive == "") != (s.StateAt == ""):
		return errors.New("state drive and mount point go together")
	}
	for _, p := range append([]string{s.StateAt}, s.Tmpfs...) {
		if p == "" {
			continue
		}
		if !mountPointRE.MatchString(p) || path.Clean(p) != p || !path.IsAbs(p) || p == "/" {
			return fmt.Errorf("invalid mount point %q", p)
		}
		for _, r := range []string{"/run/a12s", "/tmp", "/a12s", "/proc", "/sys", "/dev"} {
			if p == r || strings.HasPrefix(p, r+"/") || strings.HasPrefix(r, p+"/") {
				return fmt.Errorf("mount point %q overlaps %s", p, r)
			}
		}
	}
	return nil
}

func (d *Driver) serviceCreateOptions(spec driver.ServiceSpec, initExec string, program []string) client.ContainerCreateOptions {
	netName := d.NetworkName(spec.Name)
	mounts := []mount.Mount{{
		Type: mount.TypeVolume, Source: d.InitVolume(), Target: driver.InitDir, ReadOnly: true,
		VolumeOptions: &mount.VolumeOptions{NoCopy: true},
	}}
	if spec.StateDrive != "" {
		mounts = append(mounts, mount.Mount{
			Type: mount.TypeVolume, Source: d.VolumeName(spec.StateDrive), Target: spec.StateAt,
			VolumeOptions: &mount.VolumeOptions{NoCopy: true},
		})
	}
	tmpfs := map[string]string{
		"/run/a12s": "rw,nosuid,nodev,noexec,mode=0755,size=16m",
		"/tmp":      "rw,nosuid,nodev,mode=1777",
	}
	for _, t := range spec.Tmpfs {
		tmpfs[t] = "rw,nosuid,nodev,mode=0700"
	}
	env := append([]string{
		"A12S_SERVICE_USER=" + spec.User,
		"A12S_STATE_AT=" + spec.StateAt,
		"A12S_TMPFS=" + strings.Join(spec.Tmpfs, ","),
	}, spec.Env...)
	stop := StopTimeout
	useInit := true
	return client.ContainerCreateOptions{
		Name: d.ContainerName(spec.Name),
		Config: &container.Config{
			Image:      spec.Image,
			Hostname:   spec.Name,
			User:       "0:0", // a12s-init's bootstrap; it drops to spec.User before exec
			Entrypoint: []string{initExec, "service-init", "--"},
			Cmd:        program,
			// An image HEALTHCHECK would run image code as root; a12s runs the manifest's check.
			Healthcheck: noHealthcheck,
			Env:         env,
			StopTimeout: &stop,
			Labels: map[string]string{
				LabelFleet:      d.fleet,
				LabelService:    spec.Name,
				LabelGeneration: strconv.FormatInt(spec.Generation, 10),
				LabelRole:       RoleService,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(netName),
			RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
			Init:           &useInit,
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			CapAdd:         serviceCapAdd,
			SecurityOpt:    []string{"no-new-privileges:true"},
			Tmpfs:          tmpfs,
			Mounts:         mounts,
			Resources:      container.Resources{Memory: spec.Memory, NanoCPUs: spec.CPUMillis * 1_000_000},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
		},
	}
}

func (d *Driver) SetServicePower(ctx context.Context, name string, power driver.Power, generation int64) error {
	c, err := d.serviceContainer(ctx, name)
	if isNotFound(err) || errors.Is(err, driver.ErrNotFound) {
		return fmt.Errorf("power service %s %s: %w", name, power, driver.ErrNotStaged)
	}
	if err != nil {
		return fmt.Errorf("power service %s %s: %w", name, power, err)
	}
	switch staged := parseGeneration(c.Config.Labels); {
	case generation < staged:
		return fmt.Errorf("power service %s generation %d: generation %d is staged: %w", name, generation, staged, driver.ErrStaleGeneration)
	case generation > staged:
		return fmt.Errorf("power service %s generation %d: generation %d is staged: %w", name, generation, staged, driver.ErrNotStaged)
	}
	running := c.State != nil && (c.State.Running || c.State.Restarting || c.State.Paused)
	switch power {
	case driver.PowerAwake:
		if !running {
			if _, err := d.cli.ContainerStart(ctx, c.ID, client.ContainerStartOptions{}); err != nil {
				return fmt.Errorf("power service %s awake: %w", name, err)
			}
		}
	case driver.PowerAsleep:
		timeout := StopTimeout
		if running {
			if _, err := d.cli.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
				return fmt.Errorf("power service %s asleep: %w", name, err)
			}
		}
	default:
		return fmt.Errorf("power service %s: unknown state %q", name, power)
	}
	return nil
}

func (d *Driver) RemoveService(ctx context.Context, name string) error {
	c, err := d.serviceContainer(ctx, name)
	switch {
	case isNotFound(err) || errors.Is(err, driver.ErrNotFound):
	case err != nil:
		return fmt.Errorf("remove service %s: %w", name, err)
	default:
		if _, err := d.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
			return fmt.Errorf("remove service %s: %w", name, err)
		}
	}
	netName := d.NetworkName(name)
	nres, err := d.cli.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove service %s: %w", name, err)
	}
	if nres.Network.Labels[LabelFleet] != d.fleet {
		return fmt.Errorf("remove service %s: network %s is not owned by fleet %s", name, netName, d.fleet)
	}
	for id := range nres.Network.Containers {
		_, _ = d.cli.NetworkDisconnect(ctx, netName, client.NetworkDisconnectOptions{Container: id, Force: true})
	}
	if _, err := d.cli.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove service %s: network: %w", name, err)
	}
	return nil
}

func (d *Driver) serviceContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	res, err := d.cli.ContainerInspect(ctx, d.ContainerName(name), client.ContainerInspectOptions{})
	if isNotFound(err) {
		return container.InspectResponse{}, driver.ErrNotFound
	}
	if err != nil {
		return container.InspectResponse{}, err
	}
	if res.Container.Config.Labels[LabelFleet] != d.fleet || res.Container.Config.Labels[LabelRole] != RoleService {
		return container.InspectResponse{}, fmt.Errorf("container %s is not a Service of fleet %s", d.ContainerName(name), d.fleet)
	}
	return res.Container, nil
}

func (d *Driver) runningService(ctx context.Context, name string) (driver.ComputerState, error) {
	c, err := d.serviceContainer(ctx, name)
	if err != nil {
		return driver.ComputerState{}, err
	}
	st := d.computerState(name, c)
	if !st.Running {
		return st, fmt.Errorf("service is %s, not running", st.Power)
	}
	return st, nil
}

func (d *Driver) ServiceExec(ctx context.Context, name, user string, cmd []string, stdin io.Reader) (int, string, error) {
	if _, err := d.runningService(ctx, name); err != nil {
		return -1, "", fmt.Errorf("exec %s: %w", name, err)
	}
	return d.exec(ctx, d.ContainerName(name), user, cmd, stdin)
}

func (d *Driver) DialService(ctx context.Context, name string, port int) (net.Conn, error) {
	st, err := d.runningService(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", name, err)
	}
	addr, err := netip.ParseAddr(st.Address)
	if err != nil {
		return nil, fmt.Errorf("dial %s: no address on %s", name, st.Network)
	}
	nres, err := d.cli.NetworkInspect(ctx, st.Network, client.NetworkInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", name, err)
	}
	var subnets []netip.Prefix
	for _, c := range nres.Network.IPAM.Config {
		subnets = append(subnets, c.Subnet)
	}
	if on, err := onLocalNetwork(subnets); err != nil {
		return nil, fmt.Errorf("dial %s: %w", name, err)
	} else if !on {
		return nil, fmt.Errorf("dial %s: %w %s", name, driver.ErrNotOnNetwork, st.Network)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(port)))
}

func (d *Driver) ServiceLogs(ctx context.Context, name string, follow bool, tail int) (io.ReadCloser, error) {
	c, err := d.serviceContainer(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("logs %s: %w", name, err)
	}
	t := "all"
	if tail > 0 {
		t = strconv.Itoa(tail)
	}
	rc, err := d.cli.ContainerLogs(ctx, c.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: follow, Tail: t})
	if err != nil {
		return nil, fmt.Errorf("logs %s: %w", name, err)
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		rc.Close()
		pw.CloseWithError(err)
	}()
	return pr, nil
}

func (d *Driver) ServiceWrite(ctx context.Context, name, p, owner, mode, dirMode string, data []byte) error {
	initExec, err := d.initExec()
	if err != nil {
		return err
	}
	code, out, err := d.ServiceExec(ctx, name, "0:0", []string{initExec, "service-write", p, owner, mode, dirMode}, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("write %s in %s: %w", p, name, err)
	}
	if code != 0 {
		return fmt.Errorf("write %s in %s: exit %d: %s", p, name, code, strings.TrimSpace(out))
	}
	return nil
}
