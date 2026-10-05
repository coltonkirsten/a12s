package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// The stand-in Computer image: alpine, a TCP listener on computerd's port,
// and a clean exit on SIGTERM.
const standinDockerfile = `FROM alpine
CMD ["sh", "-c", "trap 'exit 0' TERM; nc -lk -p 7443 -e echo computerd & wait"]
`

// A HarnessBundle image FROM scratch: no shell, just /bundle.
const bundleDockerfile = `FROM scratch
COPY bundle /bundle
`

const bundleJSON = `{"kind":"codex","version":"0.0.0-test","arch":"arm64","bin":"bin/codex","signInHosts":["auth.openai.com"]}`

// TestDockerIntegration drives a real Docker Engine. Run with A12S_DOCKER_TEST=1.
func TestDockerIntegration(t *testing.T) {
	if os.Getenv("A12S_DOCKER_TEST") != "1" {
		t.Skip("set A12S_DOCKER_TEST=1 to run against Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	fleetName := "test-" + hex.EncodeToString(suffix) // objects are a12s-test-<hex>-...
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{Fleet: fleetName, Client: cli})
	if err != nil {
		t.Fatal(err)
	}
	standin := "a12s-test-standin:" + fleetName
	bundleRef := "a12s-test-bundle-codex:" + fleetName
	bundleRef2 := "a12s-test-bundle-codex:" + fleetName + "-other"
	t.Cleanup(func() { cleanup(t, cli, fleetName, standin, bundleRef, bundleRef2) })

	buildImage(ctx, t, cli, standin, map[string]string{"Dockerfile": standinDockerfile})
	buildImage(ctx, t, cli, bundleRef, map[string]string{
		"Dockerfile":         bundleDockerfile,
		"bundle/bundle.json": bundleJSON,
		"bundle/bin/codex":   "#!/bin/sh\necho fake codex\n",
	})

	// A stand-in controller container, so staging attaches it to the network.
	ctrl, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   d.ControllerName(),
		Config: &container.Config{Image: "alpine", Cmd: []string{"sleep", "600"}, Labels: map[string]string{LabelFleet: fleetName, LabelRole: RoleController}},
	})
	must(t, err)
	_, err = cli.ContainerStart(ctx, ctrl.ID, client.ContainerStartOptions{})
	must(t, err)

	caps, err := d.Capabilities(ctx)
	must(t, err)
	if !caps.EgressEnforcement || caps.DriveQuota || !caps.SharedWriteDrives || len(caps.Architectures) != 1 {
		t.Errorf("capabilities = %+v", caps)
	}
	t.Logf("capabilities: %+v", caps)

	// Drives.
	home := driver.DriveSpec{ID: driver.HomeDriveID("c"), Kind: driver.DriveHome, Computer: "c", Size: 1 << 30}
	data := driver.DriveSpec{ID: driver.WorkDriveID("data"), Kind: driver.DriveWork}
	ref := driver.DriveSpec{ID: driver.WorkDriveID("ref"), Kind: driver.DriveWork}
	for _, spec := range []driver.DriveSpec{home, data, ref} {
		created, err := d.EnsureDrive(ctx, spec)
		if err != nil || !created {
			t.Fatalf("EnsureDrive %s = %v, %v; want created", spec.ID, created, err)
		}
		if created, err = d.EnsureDrive(ctx, spec); err != nil || created {
			t.Fatalf("EnsureDrive %s again = %v, %v; want existing", spec.ID, created, err)
		}
	}

	// Bundle.
	b, err := d.EnsureBundle(ctx, bundleRef)
	must(t, err)
	if b.Manifest.Kind != "codex" || b.Manifest.Version != "0.0.0-test" || !strings.HasPrefix(b.ImageID, "sha256:") ||
		!slices.Equal(b.Manifest.SignInHosts, []string{"auth.openai.com"}) {
		t.Errorf("bundle = %+v", b)
	}
	b2, err := d.EnsureBundle(ctx, bundleRef)
	must(t, err)
	if b2.ImageID != b.ImageID {
		t.Errorf("bundle ID changed: %s %s", b.ImageID, b2.ImageID)
	}
	// A different image claiming the same kind and version gets its own directory.
	buildImage(ctx, t, cli, bundleRef2, map[string]string{
		"Dockerfile":         bundleDockerfile,
		"bundle/bundle.json": bundleJSON,
		"bundle/bin/codex":   "#!/bin/sh\necho other codex\n",
	})
	other, err := d.EnsureBundle(ctx, bundleRef2)
	must(t, err)
	if other.ImageID == b.ImageID || driver.BundleDir("codex", "0.0.0-test", other.ImageID) == driver.BundleDir("codex", "0.0.0-test", b.ImageID) {
		t.Fatalf("same-version bundles share an identity: %s %s", b.ImageID, other.ImageID)
	}

	// Stage generation 1 with a preset.
	presetDir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(presetDir, ".claude"), 0o755))
	must(t, os.WriteFile(filepath.Join(presetDir, ".claude", "settings.json"), []byte(`{"preset":true}`), 0o644))
	var preset bytes.Buffer
	must(t, fleet.WritePresetTar(presetDir, &preset))
	spec := driver.ComputerSpec{
		Name: "c", Generation: 1, Image: standin, Tier: fleet.TierCustom,
		Bundle:   driver.BundleRef{Kind: "codex", Version: "0.0.0-test", Image: b.ImageID},
		Autonomy: fleet.AutonomyFull, CPUMillis: 500, Memory: 256 << 20,
		HomeDrive:   home.ID,
		Drives:      []driver.Mount{{DriveID: data.ID, At: "/work/data"}, {DriveID: ref.ID, At: "/work/ref", ReadOnly: true}},
		Credentials: []string{"openai"},
	}

	// A preset copy that fails leaves no staged container behind, so the
	// retry below stages and copies again instead of reporting success.
	bad := spec
	bad.Preset = badPreset(t)
	if err := d.StageComputer(ctx, bad); err == nil {
		t.Fatal("stage with a broken preset succeeded")
	}
	if obs, err := d.Observe(ctx); err != nil || len(obs.Computers) != 0 {
		t.Fatalf("after a failed preset: %+v, %v", obs.Computers, err)
	}

	// A network with the Computer's name that is not isolated is never reused.
	_, err = cli.NetworkCreate(ctx, d.NetworkName("rogue"), client.NetworkCreateOptions{Driver: "bridge", Labels: map[string]string{LabelFleet: fleetName}})
	must(t, err)
	rogue := spec
	rogue.Name, rogue.HomeDrive, rogue.Drives, rogue.Preset = "rogue", home.ID, nil, nil
	if err := d.StageComputer(ctx, rogue); err == nil || !strings.Contains(err.Error(), "not internal") {
		t.Errorf("stage on a non-isolated network = %v", err)
	}

	spec.Preset = preset.Bytes()
	must(t, d.StageComputer(ctx, spec))
	must(t, d.StageComputer(ctx, spec)) // same generation: no-op

	obs, err := d.Observe(ctx)
	must(t, err)
	st := obs.Computers["c"]
	if st.Generation != 1 || st.Power != driver.PowerAsleep || st.Running || st.Failure != "" {
		t.Errorf("staged state = %+v", st)
	}
	if got := obs.Drives[data.ID].AttachedTo; !slices.Equal(got, []string{"c"}) {
		t.Errorf("data attachedTo = %v", got)
	}
	if err := d.RemoveDrive(ctx, data.ID); !errors.Is(err, driver.ErrDriveAttached) {
		t.Errorf("RemoveDrive attached = %v, want ErrDriveAttached", err)
	}

	// Power: generation checks, then awake.
	if err := d.SetPower(ctx, "c", driver.PowerAwake, 0); !errors.Is(err, driver.ErrStaleGeneration) {
		t.Errorf("SetPower gen 0 = %v, want ErrStaleGeneration", err)
	}
	if err := d.SetPower(ctx, "c", driver.PowerAwake, 2); !errors.Is(err, driver.ErrNotStaged) {
		t.Errorf("SetPower gen 2 = %v, want ErrNotStaged", err)
	}
	must(t, d.SetPower(ctx, "c", driver.PowerAwake, 1))
	must(t, d.SetPower(ctx, "c", driver.PowerAwake, 1)) // idempotent

	obs, err = d.Observe(ctx)
	must(t, err)
	st = obs.Computers["c"]
	addr, err := netip.ParseAddr(st.Address)
	if st.Power != driver.PowerAwake || !st.Running || err != nil || !DefaultSubnetPool.Contains(addr) {
		t.Fatalf("awake state = %+v", st)
	}
	if err := d.StageComputer(ctx, driver.ComputerSpec{Name: "c", Generation: 2, Image: standin, HomeDrive: home.ID, Bundle: spec.Bundle}); !errors.Is(err, driver.ErrRunning) {
		t.Errorf("stage over running = %v, want ErrRunning", err)
	}

	t.Run("container spec", func(t *testing.T) { checkContainer(ctx, t, d, cli) })
	t.Run("inside the computer", func(t *testing.T) { checkInside(ctx, t, d, st.Address, spec.Bundle.Path()) })

	// Connect: from the test process (not on the network) it must refuse clearly.
	if _, err := d.Connect(ctx, "c", driver.ServiceComputerd); !errors.Is(err, driver.ErrNotOnNetwork) {
		t.Errorf("Connect from host = %v, want ErrNotOnNetwork", err)
	} else {
		t.Logf("Connect from host: %v", err)
	}

	// Sleep, watching for the die event.
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	events, err := d.Watch(wctx)
	must(t, err)
	must(t, d.SetPower(ctx, "c", driver.PowerAsleep, 1))
	ev := nextEvent(t, events)
	if ev.Type != driver.EventDied || ev.Computer != "c" || ev.Generation != 1 {
		t.Errorf("event = %+v, want died c gen 1", ev)
	}
	obs, err = d.Observe(ctx)
	must(t, err)
	if st := obs.Computers["c"]; st.Power != driver.PowerAsleep || st.Running || st.Failure != "" {
		t.Errorf("asleep state = %+v", st)
	}

	// Stage generation 2 over the stopped Computer; home survives, preset is not reapplied.
	gen1ID := obs.Computers["c"].InstanceID
	spec.Generation = 2
	must(t, d.StageComputer(ctx, spec))
	must(t, d.SetPower(ctx, "c", driver.PowerAwake, 2))
	obs, err = d.Observe(ctx)
	must(t, err)
	if st := obs.Computers["c"]; st.Generation != 2 || st.InstanceID == gen1ID || !st.Running {
		t.Errorf("generation 2 state = %+v", st)
	}
	if _, out, _ := d.Exec(ctx, "c", []string{"cat", "/home/agent/.claude/settings.json", "/home/agent/keep"}, nil, false); out != `{"changed":true}kept`+"\n" {
		t.Errorf("home after restage = %q", out)
	}

	// Remove: container and network go, drives stay.
	must(t, d.RemoveComputer(ctx, "c"))
	must(t, d.RemoveComputer(ctx, "c")) // idempotent
	for {
		ev := nextEvent(t, events)
		if ev.Type == driver.EventRemoved && ev.Computer == "c" && ev.Generation == 2 {
			break
		}
	}
	if _, err := cli.NetworkInspect(ctx, d.NetworkName("c"), client.NetworkInspectOptions{}); !isNotFound(err) {
		t.Errorf("network still present: %v", err)
	}
	obs, err = d.Observe(ctx)
	must(t, err)
	if len(obs.Computers) != 0 || len(obs.Drives) != 3 || len(obs.Drives[home.ID].AttachedTo) != 0 {
		t.Errorf("after remove: %+v", obs)
	}
	must(t, d.RemoveDrive(ctx, data.ID))
	must(t, d.RemoveDrive(ctx, ref.ID))
	must(t, d.RemoveDrive(ctx, home.ID))
	must(t, d.RemoveDrive(ctx, home.ID)) // idempotent
}

func checkContainer(ctx context.Context, t *testing.T, d *Driver, cli *client.Client) {
	res, err := cli.ContainerInspect(ctx, d.ContainerName("c"), client.ContainerInspectOptions{})
	must(t, err)
	c := res.Container
	for k, v := range map[string]string{LabelFleet: d.fleet, LabelComputer: "c", LabelGeneration: "1", LabelRole: RoleComputer} {
		if c.Config.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, c.Config.Labels[k], v)
		}
	}
	h := c.HostConfig
	if !h.ReadonlyRootfs || h.Privileged || len(h.Binds) != 0 || len(h.PortBindings) != 0 || h.PublishAllPorts {
		t.Errorf("host config: ro=%v priv=%v binds=%v ports=%v", h.ReadonlyRootfs, h.Privileged, h.Binds, h.PortBindings)
	}
	if !slices.Equal(h.CapDrop, []string{"ALL"}) || !sameCaps(h.CapAdd, capAdd) || !slices.Contains(h.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("caps: drop=%v add=%v secopt=%v", h.CapDrop, h.CapAdd, h.SecurityOpt)
	}
	if h.Init == nil || !*h.Init {
		t.Error("init is not enabled")
	}
	if h.Memory != 256<<20 || h.NanoCPUs != 500_000_000 {
		t.Errorf("resources: memory=%d nanocpus=%d", h.Memory, h.NanoCPUs)
	}
	got := map[string]string{}
	for _, m := range c.Mounts {
		if m.Type != mount.TypeVolume {
			t.Errorf("mount %s is %s, want volume", m.Destination, m.Type)
		}
		mode := "rw"
		if !m.RW {
			mode = "ro"
		}
		got[m.Destination] = m.Name + ":" + mode
	}
	want := map[string]string{
		"/home":             d.VolumeName(driver.HomeDriveID("c")) + ":rw",
		"/opt/a12s/bundles": d.BundleVolume() + ":ro",
		"/work/data":        d.VolumeName(driver.WorkDriveID("data")) + ":rw",
		"/work/ref":         d.VolumeName(driver.WorkDriveID("ref")) + ":ro",
	}
	if !equalMaps(got, want) {
		t.Errorf("mounts = %v, want %v", got, want)
	}
	nres, err := cli.NetworkInspect(ctx, d.NetworkName("c"), client.NetworkInspectOptions{})
	must(t, err)
	n := nres.Network
	if !n.Internal || n.Options["com.docker.network.bridge.gateway_mode_ipv4"] != "isolated" ||
		len(n.IPAM.Config) != 1 || n.IPAM.Config[0].Subnet.Bits() != SubnetBits || n.Labels[LabelFleet] != d.fleet {
		t.Errorf("network: internal=%v options=%v ipam=%+v labels=%v", n.Internal, n.Options, n.IPAM.Config, n.Labels)
	}
	if len(n.Containers) != 2 {
		t.Errorf("network has %d containers, want the Computer and the controller", len(n.Containers))
	}
}

func checkInside(ctx context.Context, t *testing.T, d *Driver, address, bundlePath string) {
	run := func(asRoot bool, stdin string, cmd ...string) (int, string) {
		t.Helper()
		var in io.Reader
		if stdin != "" {
			in = strings.NewReader(stdin)
		}
		code, out, err := d.Exec(ctx, "c", cmd, in, asRoot)
		must(t, err)
		return code, out
	}
	expect := func(asRoot bool, want string, cmd ...string) {
		t.Helper()
		if code, out := run(asRoot, "", cmd...); code != 0 || strings.TrimSpace(out) != want {
			t.Errorf("%v: code %d, output %q, want %q", cmd, code, out, want)
		}
	}
	expect(false, "1000:1000", "sh", "-c", "echo $(id -u):$(id -g)")
	expect(true, "0", "id", "-u")
	// The preset landed in /home/agent, owned by the agent.
	expect(true, "1000:1000\n1000:1000", "stat", "-c", "%u:%g", "/home/agent", "/home/agent/.claude/settings.json")
	expect(false, `{"preset":true}`, "cat", "/home/agent/.claude/settings.json")
	// Agent writes for the restage check later.
	expect(false, "", "sh", "-c", `printf '{"changed":true}' > /home/agent/.claude/settings.json && echo kept > /home/agent/keep`)
	// Capabilities: exactly CHOWN, DAC_OVERRIDE, FOWNER, KILL, SETGID, SETUID; no new privileges.
	expect(true, "CapEff:\t00000000000000eb", "grep", "CapEff", "/proc/1/status")
	expect(true, "NoNewPrivs:\t1", "grep", "NoNewPrivs", "/proc/1/status")
	// Read-only rootfs and bundle cache; writable tmpfs and drives.
	if code, _ := run(true, "", "touch", "/etc/a12s-test"); code == 0 {
		t.Error("rootfs is writable")
	}
	if code, _ := run(true, "", "touch", "/opt/a12s/bundles/x"); code == 0 {
		t.Error("bundle cache is writable")
	}
	expect(false, bundleJSON, "cat", bundlePath+"/bundle.json")
	expect(false, "echo fake codex", "sed", "-n", "2p", bundlePath+"/bin/codex")
	expect(true, "", "touch", "/run/a12s/x", "/tmp/x", "/work/data/x")
	// Fresh work drives: A12S_DRIVES tells computerd which empty, root-owned
	// rw roots to hand to the agent. The stand-in has no computerd, so the
	// test does that chown through Exec, then writes as the agent.
	expect(false, "/work/data:rw,/work/ref:ro", "sh", "-c", "echo $A12S_DRIVES")
	if code, _ := run(false, "", "touch", "/work/data/agent-file"); code == 0 {
		t.Error("fresh drive root is writable by the agent before computerd's chown")
	}
	expect(true, "", "chown", "1000:1000", "/work/data")
	expect(false, "1000", "sh", "-c", "echo hi > /work/data/agent-file && stat -c %u /work/data/agent-file")
	if code, _ := run(true, "", "touch", "/work/ref/x"); code == 0 {
		t.Error("readOnly drive is writable")
	}
	expect(true, "applied", "cat", presetMarker)
	expect(true, "docker-init", "sh", "-c", "cat /proc/1/comm")
	expect(true, "tmpfs 700", "sh", "-c", "echo $(stat -f -c %T /run/a12s) $(stat -c %a /run/a12s)")
	// The secret-inject path: bytes on stdin into tmpfs.
	if code, out := run(true, "s3cret", "sh", "-c", "cat > /run/a12s/token && cat /run/a12s/token"); code != 0 || out != "s3cret" {
		t.Errorf("stdin inject: code %d, output %q", code, out)
	}
	expect(false, bundlePath, "sh", "-c", "echo $A12S_BUNDLE")
	// No route out: neither as the agent nor as root, by IP or to the host.
	for _, asRoot := range []bool{false, true} {
		if code, out := run(asRoot, "", "wget", "-q", "-T", "3", "-O-", "http://1.1.1.1"); code == 0 {
			t.Errorf("wget 1.1.1.1 succeeded (root=%v): %q", asRoot, out)
		} else {
			t.Logf("wget 1.1.1.1 (root=%v) blocked: %s", asRoot, strings.TrimSpace(out))
		}
	}
	if code, _ := run(true, "", "wget", "-q", "-T", "3", "-O-", "http://host.docker.internal"); code == 0 {
		t.Error("reached host.docker.internal")
	}
	if code, out := run(true, "", "ip", "route"); code != 0 || strings.Contains(out, "default") {
		t.Errorf("unexpected routes: %q", out)
	}
	// The controller resolves by name on the Computer network and reaches computerd's port.
	expect(true, "", "sh", "-c", "nslookup "+d.ControllerName()+" >/dev/null && echo -n")
	code, out, err := execIn(ctx, d.cli, d.ControllerName(), "nc", "-w", "3", address, "7443")
	if err != nil || code != 0 || strings.TrimSpace(out) != "computerd" {
		t.Errorf("controller -> computerd: %d %q %v", code, out, err)
	}
}

func execIn(ctx context.Context, cli *client.Client, name string, cmd ...string) (int, string, error) {
	created, err := cli.ExecCreate(ctx, name, client.ExecCreateOptions{Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return -1, "", err
	}
	att, err := cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return -1, "", err
	}
	defer att.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, att.Reader)
	res, err := cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return -1, "", err
	}
	out := buf.String()
	if len(out) > 8 { // strip the 8-byte stream header of the single frame
		out = out[8:]
	}
	return res.ExitCode, out, nil
}

// badPreset is a preset tar the driver refuses (a hard link), so the copy fails.
func badPreset(t *testing.T) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	must(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "l", Linkname: "x"}))
	must(t, tw.Close())
	return buf.Bytes()
}

func nextEvent(t *testing.T, events <-chan driver.Event) driver.Event {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("watch closed")
		}
		return ev
	case <-time.After(30 * time.Second):
		t.Fatal("no event within 30s")
	}
	return driver.Event{}
}

func buildImage(ctx context.Context, t *testing.T, cli *client.Client, tag string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		mode := int64(0o644)
		if strings.Contains(name, "/bin/") {
			mode = 0o755
		}
		must(t, tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(body))}))
		_, err := tw.Write([]byte(body))
		must(t, err)
	}
	must(t, tw.Close())
	res, err := cli.ImageBuild(ctx, &buf, client.ImageBuildOptions{Tags: []string{tag}, Remove: true, ForceRemove: true})
	must(t, err)
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	must(t, err)
	if bytes.Contains(out, []byte(`"errorDetail"`)) {
		t.Fatalf("build %s: %s", tag, out)
	}
}

// cleanup removes everything labeled with the test Fleet, and the test images.
func cleanup(t *testing.T, cli *client.Client, fleetName string, images ...string) {
	ctx := context.Background()
	f := make(client.Filters).Add("label", LabelFleet+"="+fleetName)
	if list, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: f}); err == nil {
		for _, c := range list.Items {
			_, _ = cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
		}
	}
	if list, err := cli.NetworkList(ctx, client.NetworkListOptions{Filters: f}); err == nil {
		for _, n := range list.Items {
			_, _ = cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
		}
	}
	if list, err := cli.VolumeList(ctx, client.VolumeListOptions{Filters: f}); err == nil {
		for _, v := range list.Items {
			if _, err := cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil {
				t.Logf("cleanup volume %s: %v", v.Name, err)
			}
		}
	}
	for _, img := range images {
		_, _ = cli.ImageRemove(ctx, img, client.ImageRemoveOptions{Force: true, PruneChildren: true})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// sameCaps compares capability lists ignoring order and the CAP_ prefix the engine adds.
func sameCaps(a, b []string) bool {
	norm := func(caps []string) []string {
		out := make([]string, len(caps))
		for i, c := range caps {
			out[i] = strings.TrimPrefix(c, "CAP_")
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(norm(a), norm(b))
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
