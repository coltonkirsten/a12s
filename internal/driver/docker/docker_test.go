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

	"github.com/moby/moby/api/types/mount"

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

func TestRebasePresetTar(t *testing.T) {
	in := makeTar(t, entry{name: ".claude/"}, entry{name: ".claude/settings.json", body: "{}"}, entry{name: "../../etc/evil", body: "x"})
	var out bytes.Buffer
	if err := rebasePresetTar(bytes.NewReader(in), &out, "agent"); err != nil {
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
		Bundle:   driver.BundleRef{Kind: "claude-code", Version: "2.1.280"},
		Model:    "claude-fable-5-1",
		Autonomy: "full", CPUMillis: 1500, Memory: 2 << 30,
		HomeDrive:   driver.HomeDriveID("scout"),
		Drives:      []driver.Mount{{DriveID: driver.WorkDriveID("reference"), At: "/work/ref", ReadOnly: true}},
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
		"A12S_HARNESS_KIND=claude-code", "A12S_HARNESS_VERSION=2.1.280", "A12S_AUTONOMY=full",
		"A12S_IMAGE_TIER=slim", "A12S_BUNDLE=/opt/a12s/bundles/claude-code-2.1.280",
		"A12S_CREDENTIALS=anthropic,github",
		"A12S_PROXY=http://a12s-lab-controller:3128", "A12S_CONTROLLER=https://a12s-lab-controller:7443",
		"A12S_MODEL=claude-fable-5-1",
	}
	if !reflect.DeepEqual(o.Config.Env, wantEnv) {
		t.Errorf("env = %q", o.Config.Env)
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
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Errorf("mounts = %+v", mounts)
	}
}
