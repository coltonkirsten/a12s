package engine

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/coltonkirsten/a12s/internal/driver"
)

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
	if ev, ok := ToEvent(msg(RoleComputer, "scout")); !ok || ev.Service || ev.Computer != "scout" || ev.Generation != 3 || ev.ExitCode != 1 {
		t.Fatalf("computer: %+v %v", ev, ok)
	}
	if ev, ok := ToEvent(msg(RoleService, "clock")); !ok || !ev.Service || ev.Computer != "clock" {
		t.Fatalf("service: %+v %v", ev, ok)
	}
	if _, ok := ToEvent(msg(RoleController, "clock")); ok {
		t.Fatal("the Controller is neither a Computer nor a Service")
	}
}

func TestCheckSpecAttach(t *testing.T) {
	base := driver.ComputerSpec{
		Name: "brain", Generation: 1, Image: "a12s/slim:dev", HomeDrive: driver.HomeDriveID("brain"),
		Bundle: driver.BundleRef{Kind: "claude-code", Version: "2.1.280", Image: "sha256:0123456789abcdef0123"},
	}
	for _, a := range []driver.Attach{
		{Host: "app", Target: "/home/agent/app"},
		{Host: "/", Target: "/home/agent/app"},
		{Host: "/x/../y", Target: "/home/agent/app"},
		{Host: "/x", Target: "/home/agent"},
		{Host: "/x", Target: "/home/agent/a/b"},
		{Host: "/x", Target: "/run/a12s"},
		{Host: "/x", Target: "/home/agent/app", Keep: []driver.Keep{{Drive: "d", Target: "/home/agent/other"}}},
		{Host: "/x", Target: "/home/agent/app", Keep: []driver.Keep{{Drive: "d", Target: "/home/agent/app/../../.."}}},
		{Host: "/x", Target: "/home/agent/app", Keep: []driver.Keep{{Target: "/home/agent/app/node_modules"}}},
	} {
		s := base
		s.Attach = []driver.Attach{a}
		if err := CheckSpec(s); err == nil {
			t.Errorf("checkSpec accepted %+v", a)
		}
	}
}

func TestExplainMountError(t *testing.T) {
	attach := []driver.Attach{{Host: "/Users/me/app", Target: "/home/agent/project"}, {Host: "/Users/me/app2", Target: "/home/agent/second"}, {Host: "/opt/data", Target: "/home/agent/data"}}
	for msg, want := range map[string]string{
		`invalid mount config for type "bind": bind source path does not exist: /host_mnt/Users/me/app`: "attach.project (/Users/me/app)",
		`invalid mount config for type "bind": bind source path does not exist: /opt/data`:              "attach.data (/opt/data)",
		`invalid mount config for type "bind": bind source path does not exist: /Users/me/app2`:         "attach.second (/Users/me/app2)",
		`mounts denied: The path /elsewhere is not shared from the host and is not known to Docker.`:    "an attached folder",
		`Error response from daemon: No such image: a12s/slim:dev`:                                      "",
	} {
		got := ExplainMountError(errors.New(msg), attach).Error()
		if want == "" && got != msg || want != "" && (!strings.Contains(got, "Docker can't see "+want) || !strings.Contains(got, "File sharing")) {
			t.Errorf("%s\n -> %s, want %q", msg, got, want)
		}
	}
}

// Regression (first real GCE run): a published port must also be exposed, or the engine never binds it.
func TestCreateOptionsExposesPublishedPorts(t *testing.T) {
	port := network.MustParsePort("7443/tcp")
	p := Placement{
		Name: "c", Network: "bridge",
		Drive: func(_, target string) mount.Mount {
			return mount.Mount{Type: mount.TypeBind, Source: "/h", Target: target}
		},
		Ports: network.PortMap{port: {{HostPort: "7443"}}},
	}
	opts := (&Engine{}).CreateOptions(driver.ComputerSpec{Name: "c"}, p)
	if _, ok := opts.Config.ExposedPorts[port]; !ok {
		t.Fatalf("ExposedPorts = %v, want %v", opts.Config.ExposedPorts, port)
	}
	if got := (&Engine{}).CreateOptions(driver.ComputerSpec{Name: "c"}, Placement{Drive: p.Drive}).Config.ExposedPorts; got != nil {
		t.Fatalf("no ports: ExposedPorts = %v, want nil", got)
	}
}
