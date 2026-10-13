package docker

import (
	"archive/tar"
	"bytes"
	"io"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/coltonkirsten/a12s/internal/driver"
)

func TestAllocateSubnet(t *testing.T) {
	pool := netip.MustParsePrefix("10.213.0.0/16")
	p := netip.MustParsePrefix
	tests := []struct {
		name string
		used []netip.Prefix
		want string
	}{
		{"empty pool", nil, "10.213.0.0/29"},
		{"skip used", []netip.Prefix{p("10.213.0.0/29"), p("10.213.0.8/29")}, "10.213.0.16/29"},
		{"reuse a gap", []netip.Prefix{p("10.213.0.0/29"), p("10.213.0.16/29")}, "10.213.0.8/29"},
		{"skip a larger overlapping network", []netip.Prefix{p("10.213.0.0/24")}, "10.213.1.0/29"},
		{"ignore networks outside the pool", []netip.Prefix{p("172.17.0.0/16"), p("10.0.0.0/24")}, "10.213.0.0/29"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := allocateSubnet(pool, SubnetBits, tt.used)
			if err != nil || got.String() != tt.want {
				t.Fatalf("allocateSubnet = %v, %v; want %s", got, err, tt.want)
			}
		})
	}
	if _, err := allocateSubnet(p("10.213.0.0/28"), SubnetBits, []netip.Prefix{p("10.213.0.0/28")}); err == nil {
		t.Error("exhausted pool should fail")
	}
}

type entry struct {
	name     string
	uid, gid int
	body     string
}

func readTar(t *testing.T, r io.Reader) []entry {
	t.Helper()
	var out []entry
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out = append(out, entry{h.Name, h.Uid, h.Gid, string(body)})
	}
}

func makeTar(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Uid: e.uid, Gid: e.gid, Typeflag: tar.TypeReg}
		if strings.HasSuffix(e.name, "/") {
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRebaseFilesTar(t *testing.T) {
	in := makeTar(t, entry{name: ".claude/"}, entry{name: ".claude/settings.json", body: "{}"}, entry{name: "../../etc/evil", body: "x"})
	var out bytes.Buffer
	if err := rebaseFilesTar(bytes.NewReader(in), &out, "agent"); err != nil {
		t.Fatal(err)
	}
	want := []entry{
		{"agent/", 1000, 1000, ""},
		{"agent/.claude/", 1000, 1000, ""},
		{"agent/.claude/settings.json", 1000, 1000, "{}"},
		{"agent/etc/evil", 1000, 1000, "x"}, // cleaned, cannot escape agent/
	}
	if got := readTar(t, &out); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestBundleDir(t *testing.T) {
	id := "sha256:3f1c9a0b7d2e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a"
	if got := driver.BundleDir("codex", "0.154.0", id); got != "codex-0.154.0-3f1c9a0b7d2e" {
		t.Errorf("BundleDir = %q", got)
	}
	if got := driver.BundlePath("codex", "0.154.0", id); got != "/opt/a12s/bundles/codex-0.154.0-3f1c9a0b7d2e" {
		t.Errorf("BundlePath = %q", got)
	}
	for _, bad := range []string{"", "a12s/bundle-codex:0.154.0", "sha256:short", "sha256:XYZ456789abcdef"} {
		if driver.ShortImageID(bad) != "" {
			t.Errorf("ShortImageID(%q) should be empty", bad)
		}
	}
	spec := driver.ComputerSpec{Name: "c", Generation: 1, Image: "x", HomeDrive: "home-c",
		Bundle: driver.BundleRef{Kind: "codex", Version: "1", Image: "a12s/bundle-codex:1"}}
	if err := checkSpec(spec); err == nil || !strings.Contains(err.Error(), "image ID") {
		t.Errorf("checkSpec with a bundle ref = %v, want an image ID error", err)
	}
}

func TestCheckSpecMountPoints(t *testing.T) {
	base := driver.ComputerSpec{Name: "c", Generation: 1, Image: "x", HomeDrive: "home-c",
		Bundle: driver.BundleRef{Kind: "codex", Version: "1", Image: "sha256:0123456789abcdef"}}
	for at, ok := range map[string]bool{
		"/work/data":          true,
		"/work/A-z_0.9":       true,
		"/work/v,/home/.a12s": false,
		"/work/v:rw":          false,
		"/work/my dir":        false,
		"work":                false,
		"/work/../home":       false,
		"/":                   false,
		"/home/agent/x":       false,
		"/run/a12s":           false,
		"/opt/a12s/bundles/x": false,
		"/tmp/x":              false,
		"/proc/x":             false,
		"/sys":                false,
		"/dev/shm":            false,
		"/homework":           true,
	} {
		s := base
		s.Drives = []driver.Mount{{DriveID: "drive-d", At: at}}
		if err := checkSpec(s); (err == nil) != ok {
			t.Errorf("checkSpec(at=%q) = %v, want ok=%v", at, err, ok)
		}
	}
}

func TestCheckNetwork(t *testing.T) {
	d := &Driver{fleet: "lab"}
	good := network.Network{
		Name: "a12s-lab-c", Internal: true,
		Options: map[string]string{gatewayModeOption: "isolated"},
		Labels:  map[string]string{LabelFleet: "lab", LabelComputer: "c", LabelRole: RoleComputer},
	}
	if err := d.checkNetwork(good, "c", RoleComputer); err != nil {
		t.Fatal(err)
	}
	mutate := map[string]func(n *network.Network){
		"not internal":   func(n *network.Network) { n.Internal = false },
		"nat gateway":    func(n *network.Network) { n.Options = map[string]string{gatewayModeOption: "nat"} },
		"no gateway opt": func(n *network.Network) { n.Options = nil },
		"other fleet": func(n *network.Network) {
			n.Labels = map[string]string{LabelFleet: "x", LabelComputer: "c", LabelRole: RoleComputer}
		},
		"other computer": func(n *network.Network) {
			n.Labels = map[string]string{LabelFleet: "lab", LabelComputer: "d", LabelRole: RoleComputer}
		},
		"no role": func(n *network.Network) { n.Labels = map[string]string{LabelFleet: "lab", LabelComputer: "c"} },
	}
	for name, m := range mutate {
		n := good
		m(&n)
		if err := d.checkNetwork(n, "c", RoleComputer); err == nil {
			t.Errorf("%s: want rejection", name)
		}
	}
}

func TestRebaseBundleTar(t *testing.T) {
	in := makeTar(t,
		entry{name: "bundle/"},
		entry{name: "bundle/bundle.json", body: `{"kind":"codex"}`, uid: 5},
		entry{name: "bundle/bin/"},
		entry{name: "bundle/bin/codex", body: "ELF", uid: 5},
	)
	var out bytes.Buffer
	if err := rebaseBundleTar(bytes.NewReader(in), &out, "bundle", "codex-0.154.0"); err != nil {
		t.Fatal(err)
	}
	want := []entry{
		{"codex-0.154.0/", 0, 0, ""},
		{"codex-0.154.0/bin/", 0, 0, ""},
		{"codex-0.154.0/bin/codex", 0, 0, "ELF"},
		{"codex-0.154.0/bundle.json", 0, 0, `{"kind":"codex"}`}, // last: marks completion
	}
	if got := readTar(t, &out); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}

	bad := makeTar(t, entry{name: "bundlex/bundle.json", body: "{}"})
	if err := rebaseBundleTar(bytes.NewReader(bad), io.Discard, "bundle", "x-1"); err == nil {
		t.Error("entry outside bundle/ should fail")
	}
}

func TestCreateOptions(t *testing.T) {
	d := &Driver{fleet: "lab"}
	spec := driver.ComputerSpec{
		Name: "scout", Generation: 3, Image: "a12s/slim:dev", Tier: "slim",
		Bundle:    driver.BundleRef{Kind: "claude-code", Version: "2.1.280", Image: "sha256:0123456789abcdef0123"},
		Model:     "claude-fable-5-1",
		CPUMillis: 1500, Memory: 2 << 30,
		HomeDrive: driver.HomeDriveID("scout"),
		Drives: []driver.Mount{
			{DriveID: driver.WorkDriveID("reference"), At: "/work/ref", ReadOnly: true},
			{DriveID: driver.WorkDriveID("scratch"), At: "/work/scratch"},
		},
		Credentials: []string{"anthropic", "github"},
	}
	o := d.createOptions(spec)
	if o.Name != "a12s-lab-scout" || string(o.HostConfig.NetworkMode) != "a12s-lab-scout" {
		t.Errorf("names: %s %s", o.Name, o.HostConfig.NetworkMode)
	}
	wantLabels := map[string]string{LabelFleet: "lab", LabelComputer: "scout", LabelGeneration: "3", LabelRole: RoleComputer}
	if !reflect.DeepEqual(o.Config.Labels, wantLabels) {
		t.Errorf("labels = %v", o.Config.Labels)
	}
	wantEnv := []string{
		"A12S_COMPUTER=scout", "A12S_FLEET=lab", "A12S_GENERATION=3",
		"A12S_HARNESS_KIND=claude-code", "A12S_HARNESS_VERSION=2.1.280",
		"A12S_IMAGE_TIER=slim", "A12S_BUNDLE=/opt/a12s/bundles/claude-code-2.1.280-0123456789ab",
		"A12S_DRIVES=/work/ref:ro,/work/scratch:rw",
		"A12S_CREDENTIALS=anthropic,github",
		"A12S_PROXY=http://a12s-lab-controller:3128", "A12S_CONTROLLER=https://a12s-lab-controller:7443",
		"A12S_MODEL=claude-fable-5-1",
	}
	// Compared as a set: order and extra variables are not this test's concern.
	for _, kv := range wantEnv {
		if !slices.Contains(o.Config.Env, kv) {
			t.Errorf("env lacks %s: %q", kv, o.Config.Env)
		}
	}
	h := o.HostConfig
	if !h.ReadonlyRootfs || !reflect.DeepEqual(h.CapDrop, []string{"ALL"}) ||
		!reflect.DeepEqual(h.CapAdd, []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETUID", "SETGID", "KILL"}) ||
		!slices.Contains(h.SecurityOpt, "no-new-privileges:true") {
		t.Errorf("security: ro=%v drop=%v add=%v opt=%v", h.ReadonlyRootfs, h.CapDrop, h.CapAdd, h.SecurityOpt)
	}
	if len(h.Binds) != 0 || len(h.PortBindings) != 0 || h.Privileged {
		t.Error("no binds, ports or privilege expected")
	}
	if _, ok := h.Tmpfs["/run/a12s"]; !ok {
		t.Error("tmpfs /run/a12s missing")
	}
	if h.Init == nil || !*h.Init {
		t.Error("init not set")
	}
	if _, ok := h.Tmpfs["/tmp"]; !ok {
		t.Error("tmpfs /tmp missing")
	}
	if h.Memory != 2<<30 || h.NanoCPUs != 1_500_000_000 {
		t.Errorf("resources: %d %d", h.Memory, h.NanoCPUs)
	}
	type m struct {
		src, dst string
		ro       bool
	}
	var mounts []m
	for _, x := range h.Mounts {
		if x.Type != mount.TypeVolume || x.VolumeOptions == nil || !x.VolumeOptions.NoCopy {
			t.Errorf("mount %s: want a no-copy volume", x.Target)
		}
		mounts = append(mounts, m{x.Source, x.Target, x.ReadOnly})
	}
	wantMounts := []m{
		{"a12s-lab-home-scout", "/home", false},
		{"a12s-lab-bundles", "/opt/a12s/bundles", true},
		{"a12s-lab-drive-reference", "/work/ref", true},
		{"a12s-lab-drive-scratch", "/work/scratch", false},
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Errorf("mounts = %+v", mounts)
	}
}

func TestToEventRoles(t *testing.T) {
	msg := func(role, name string) events.Message {
		attrs := map[string]string{LabelRole: role, LabelGeneration: "3", "exitCode": "1"}
		if role == RoleService {
			attrs[LabelService] = name
		} else {
			attrs[LabelComputer] = name
		}
		return events.Message{Action: events.ActionDie, Actor: events.Actor{Attributes: attrs}}
	}
	if ev, ok := toEvent(msg(RoleComputer, "scout")); !ok || ev.Service || ev.Computer != "scout" || ev.Generation != 3 || ev.ExitCode != 1 {
		t.Fatalf("computer: %+v %v", ev, ok)
	}
	if ev, ok := toEvent(msg(RoleService, "clock")); !ok || !ev.Service || ev.Computer != "clock" {
		t.Fatalf("service: %+v %v", ev, ok)
	}
	if _, ok := toEvent(msg(RolePublish, "clock")); ok {
		t.Fatal("a publish forwarder is not a Service")
	}
}

func TestServiceContainerHardening(t *testing.T) {
	d := &Driver{fleet: "lab"}
	o := d.serviceCreateOptions(driver.ServiceSpec{Name: "clock", Generation: 1, Image: "sha256:x", User: "1000:1000", Tmpfs: []string{"/cache"}}, "/a12s/init/abc/a12s", []string{"run"})
	if o.Config.Healthcheck == nil || len(o.Config.Healthcheck.Test) != 1 || o.Config.Healthcheck.Test[0] != "NONE" {
		t.Fatal("image HEALTHCHECK not disabled")
	}
	h := o.HostConfig
	if !h.ReadonlyRootfs || h.RestartPolicy.Name != "no" || len(h.CapDrop) != 1 || h.CapDrop[0] != "ALL" || h.Tmpfs["/cache"] == "" || h.Privileged {
		t.Fatalf("host config %+v", h)
	}
	for _, m := range h.Mounts {
		if m.Target == driver.InitDir && !m.ReadOnly {
			t.Fatal("init volume writable")
		}
	}
}
