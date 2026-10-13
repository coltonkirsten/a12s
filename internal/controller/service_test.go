package controller

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

const testManifest = `{"a12s":1,"name":"svc-test","version":"0.1.0","user":1000,
"health":{"exec":["true"]},"ports":{"http":{"port":8080}},
"state":{"at":"/var/lib/svc","size":"1Gi"}}`

// withServices adds a Service image to the fake driver and returns a lab
// Fleet with one Service, clock, that sends to scout.
func withServices(t *testing.T, d *fakeDriver, edit func(f *fleet.Fleet)) *fleet.Fleet {
	t.Helper()
	d.mu.Lock()
	d.images["a12s/svc-test:0.1"] = "sha256:svctest01aaaaaaaa"
	d.images["a12s/svc-test:0.2"] = "sha256:svctest02bbbbbbbb"
	d.svcImages["sha256:svctest01aaaaaaaa"] = driver.ServiceImage{Manifest: []byte(testManifest), HasCommand: true}
	d.svcImages["sha256:svctest02bbbbbbbb"] = driver.ServiceImage{Manifest: []byte(testManifest), HasCommand: true}
	d.images["postgres:17"] = "sha256:postgres17cccccc"
	d.svcImages["sha256:postgres17cccccc"] = driver.ServiceImage{Volumes: []string{"/var/lib/postgresql/data"}, HasCommand: true}
	d.mu.Unlock()
	return labFleet(t, func(f *fleet.Fleet) {
		f.Services = map[string]fleet.Service{
			"clock": {Image: "a12s/svc-test:0.1", Grants: fleet.Grants{Send: []string{"scout"}}, Config: map[string]any{"env": map[string]any{"TZ": "UTC"}}},
		}
		if edit != nil {
			edit(f)
		}
	})
}

func (h *harness) waitService(name string, want api.Phase) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.c.servicePhase(name) != want {
		if time.Now().After(deadline) {
			h.t.Fatalf("service %s: phase %s, want %s\nlog:\n%s", name, h.c.servicePhase(name), want, h.log.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestServiceLifecycle(t *testing.T) {
	h := newHarness(t)
	f := withServices(t, h.d, nil)
	res := h.up(f, nil, false)
	if !slices.Equal(res.Services.Created, []string{"clock"}) || len(res.Errors) > 0 {
		t.Fatalf("up: %+v", res)
	}
	if res.Lock.Services["clock@a12s/svc-test:0.1"] != "sha256:svctest01aaaaaaaa" {
		t.Fatalf("lock %v", res.Lock.Services)
	}
	h.waitService("clock", api.PhaseReady)
	s := h.d.svc("clock")
	if s.spec.User != "1000:1000" || s.spec.StateDrive != "state-clock" || s.spec.StateAt != "/var/lib/svc" {
		t.Fatalf("spec %+v", s.spec)
	}
	if !h.d.hasDrive("state-clock") {
		t.Fatal("no state drive")
	}
	// Config and .generation were injected through a12s-init; .generation last.
	var cfg map[string]any
	if err := json.Unmarshal(s.files["/run/a12s/config/config.json"], &cfg); err != nil || cfg["env"].(map[string]any)["TZ"] != "UTC" {
		t.Fatalf("config %s (%v)", s.files["/run/a12s/config/config.json"], err)
	}
	if string(s.files["/run/a12s/secrets/.generation"]) == "" {
		t.Fatal("no .generation")
	}
	env := strings.Join(s.spec.Env, " ")
	for _, want := range []string{"A12S_SERVICE=clock", "A12S_API=https://a12s-lab-controller:7443", "HTTPS_PROXY=http://a12s-lab-controller:3128", "A12S_GENERATION=1"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env lacks %s: %s", want, env)
		}
	}

	// Status lists it, with the grants and the pin.
	st := h.c.Status(context.Background())
	if len(st.Services) != 1 || st.Services[0].Phase != api.PhaseReady || st.Services[0].Pin != "sha256:svctest01aaaaaaaa" || st.Services[0].Send[0] != "scout" {
		t.Fatalf("status %+v", st.Services)
	}

	// It exits: the Controller restarts it through the full start path.
	h.d.killService("clock", 1)
	waitFor(t, func() bool { return h.c.serviceStatus("clock").Restarts == 1 })
	h.waitService("clock", api.PhaseReady)
	if !h.d.svc("clock").running {
		t.Fatal("not restarted")
	}

	// Health fails: Unhealthy, reported, not restarted.
	h.d.mu.Lock()
	h.d.svcUnhealthy["clock"] = true
	h.d.mu.Unlock()
	h.waitService("clock", api.PhaseUnhealthy)
	if !h.d.svc("clock").running {
		t.Fatal("an Unhealthy Service must keep running")
	}
	h.d.mu.Lock()
	h.d.svcUnhealthy["clock"] = false
	h.d.mu.Unlock()
	h.waitService("clock", api.PhaseReady)

	// Sleep and wake; desired power survives a Controller restart.
	if err := h.c.SetServicePower(context.Background(), "clock", api.ServicePowerRequest{State: api.PowerAsleep}); err != nil {
		t.Fatal(err)
	}
	h.waitService("clock", api.PhaseAsleep)
	if h.d.svc("clock").running {
		t.Fatal("asleep but running")
	}
	h = h.restart()
	h.waitService("clock", api.PhaseAsleep)
	if err := h.c.SetServicePower(context.Background(), "clock", api.ServicePowerRequest{State: api.PowerAwake, Credentials: labCreds}); err != nil {
		t.Fatal(err)
	}
	h.waitService("clock", api.PhaseReady)

	// Audit lines are kept for `a12s service logs`.
	var out strings.Builder
	if err := h.c.writeServiceLogs(context.Background(), "clock", false, 0, func(s string) error { out.WriteString(s); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"started generation 1", "exited (code 1); restart 1", "Unhealthy: health: exit 1", "asleep (state drive kept)", "hello from clock"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("logs lack %q:\n%s", want, out.String())
		}
	}

	// Removed from the Fleet: stopped and deleted, its state drive kept as
	// an orphan; down/up would keep it too (only drive rm destroys it).
	f2 := withServices(t, h.d, func(f *fleet.Fleet) { f.Services = nil })
	res = h.up(f2, res.Lock, false)
	if !slices.Equal(res.Services.Removed, []string{"clock"}) {
		t.Fatalf("removed %+v", res.Services)
	}
	if h.d.svc("clock") != nil || !h.d.hasDrive("state-clock") {
		t.Fatal("removal must delete the container and keep the state drive")
	}
	drives, _ := h.c.drives(context.Background())
	var row api.DriveRow
	for _, d := range drives {
		if d.Name == "clock-state" {
			row = d
		}
	}
	if row.Kind != api.DriveState || !row.Orphaned || row.Service != "clock" {
		t.Fatalf("state drive row %+v", row)
	}
}

func TestServicePinning(t *testing.T) {
	h := newHarness(t)
	f := withServices(t, h.d, nil)
	res := h.up(f, nil, false)
	h.waitService("clock", api.PhaseReady)
	lock := res.Lock

	// A changed ref appends a key; the old pin is never rewritten.
	f2 := withServices(t, h.d, func(f *fleet.Fleet) {
		s := f.Services["clock"]
		s.Image = "a12s/svc-test:0.2"
		f.Services["clock"] = s
	})
	res = h.up(f2, lock, false)
	if !slices.Equal(res.Services.Updated, []string{"clock"}) {
		t.Fatalf("updated %+v", res.Services)
	}
	if res.Lock.Services["clock@a12s/svc-test:0.1"] != "sha256:svctest01aaaaaaaa" || res.Lock.Services["clock@a12s/svc-test:0.2"] != "sha256:svctest02bbbbbbbb" {
		t.Fatalf("lock %v", res.Lock.Services)
	}
	h.waitService("clock", api.PhaseReady)
	if s := h.d.svc("clock"); s.spec.Image != "sha256:svctest02bbbbbbbb" || s.spec.Generation != 2 {
		t.Fatalf("runs %s gen %d", s.spec.Image, s.spec.Generation)
	}

	// The tag moves: up keeps the pin; lock update moves it explicitly.
	h.d.mu.Lock()
	h.d.images["a12s/svc-test:0.2"] = "sha256:svctest03dddddddd"
	h.d.images["<none>"] = "sha256:svctest02bbbbbbbb" // the old image stays, untagged
	h.d.svcImages["sha256:svctest03dddddddd"] = driver.ServiceImage{Manifest: []byte(testManifest), HasCommand: true}
	h.d.mu.Unlock()
	res = h.up(f2, res.Lock, false)
	if res.Lock.Services["clock@a12s/svc-test:0.2"] != "sha256:svctest02bbbbbbbb" || len(res.Services.Updated) != 0 {
		t.Fatalf("up moved a pin: %v %+v", res.Lock.Services, res.Services)
	}
	lock = res.Lock
	delete(lock.Services, "clock@a12s/svc-test:0.2")
	r, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f2), Lock: lock, Credentials: labCreds, Refresh: []string{"clock"}}, writeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Lock.Services["clock@a12s/svc-test:0.2"] != "sha256:svctest03dddddddd" {
		t.Fatalf("lock update: %v", r.Lock.Services)
	}
	h.waitService("clock", api.PhaseReady)
	if h.d.svc("clock").spec.Image != "sha256:svctest03dddddddd" {
		t.Fatal("lock update did not run the new digest")
	}
}

func TestServiceAdmission(t *testing.T) {
	h := newHarness(t)
	// A stock image with an inline manifest that leaves a VOLUME uncovered.
	f := withServices(t, h.d, func(f *fleet.Fleet) {
		f.Services["pg"] = fleet.Service{Image: "postgres:17", Manifest: &fleet.ServiceManifest{
			User:   &fleet.ServiceUser{UID: 999, GID: 999},
			Health: &fleet.ServiceHealth{Exec: []string{"pg_isready"}},
		}}
	})
	_, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f), Credentials: labCreds}, writeOpts{})
	if err == nil || !strings.Contains(err.Error(), "services.pg") || !strings.Contains(err.Error(), "/var/lib/postgresql/data") || !strings.Contains(err.Error(), "fix:") {
		t.Fatalf("uncovered volume: %v", err)
	}
	// A static credential with no material is refused at admission...
	f = withServices(t, h.d, func(f *fleet.Fleet) {
		s := f.Services["clock"]
		s.Credentials = []string{"postgres"}
		f.Services["clock"] = s
	})
	_, err = h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f), Credentials: labCreds}, writeOpts{})
	if err == nil || !strings.Contains(err.Error(), "services.clock: ") || !strings.Contains(err.Error(), "postgres") || !strings.Contains(err.Error(), "fix: a12s creds set postgres") {
		t.Fatalf("missing credential: %v", err)
	}
	// ...but one the Service's own login produces leaves only it waiting.
	f = withServices(t, h.d, func(f *fleet.Fleet) {
		s := f.Services["clock"]
		s.Credentials = []string{"gh-oauth"}
		s.Manifest = &fleet.ServiceManifest{Login: &fleet.ServiceLogin{Command: []string{"login"}, Produces: "gh-oauth"}}
		f.Services["clock"] = s
	})
	res, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f), Credentials: labCreds}, writeOpts{})
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("login credential: %v %+v", err, res.Errors)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "fix: a12s service login clock") }) {
		t.Fatalf("warnings %v", res.Warnings)
	}
	h.waitService("clock", api.PhaseError)
	h.waitPhase("scout", api.PhaseReady) // the rest of the Fleet runs
}

func TestServiceLogsFollow(t *testing.T) {
	h := newHarness(t)
	h.up(withServices(t, h.d, nil), nil, false)
	h.waitService("clock", api.PhaseReady)
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	go func() {
		_ = h.c.writeServiceLogs(ctx, "clock", true, 0, func(s string) error { _, err := pw.Write([]byte(s)); return err })
		pw.Close()
	}()
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			b.Write(buf[:n])
			if strings.Contains(b.String(), "followed line") || err != nil {
				got <- b.String()
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	h.c.auditf("clock", "followed line")
	select {
	case s := <-got:
		if !strings.Contains(s, "followed line") {
			t.Fatalf("follow: %s", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow did not deliver the audit line")
	}
	cancel()
	pr.Close()
}
