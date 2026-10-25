package gce

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gcefake"
)

// fakeGuests stands in for each VM's engine. A container outlives its VM's stop, as it does on the boot disk, but
// stops running.
type fakeGuests struct {
	mu      sync.Mutex
	vms     map[string]*fakeVM // by Computer
	failMat error
	opened  int
}

type fakeVM struct {
	boot     string
	exists   bool
	running  bool
	spec     driver.ComputerSpec
	built    int // containers materialized
	events   chan driver.Event
	execs    []string
	sessions int
}

func (f *fakeGuests) open(_ context.Context, vm VM) (Guest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	v := f.vms[vm.Computer]
	if v == nil {
		v = &fakeVM{events: make(chan driver.Event, 8)}
		f.vms[vm.Computer] = v
	}
	if v.boot != vm.Started {
		v.boot, v.running = vm.Started, false
	}
	return &fakeGuest{f: f, vm: v}, nil
}

func (f *fakeGuests) get(computer string) fakeVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v := f.vms[computer]; v != nil {
		return *v
	}
	return fakeVM{}
}

type fakeGuest struct {
	f  *fakeGuests
	vm *fakeVM
}

func (g *fakeGuest) Materialize(_ context.Context, spec driver.ComputerSpec) error {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	if g.f.failMat != nil {
		return g.f.failMat
	}
	if g.vm.exists && g.vm.spec.Generation == spec.Generation && g.vm.spec.FilesDigest == spec.FilesDigest {
		return nil
	}
	g.vm.exists, g.vm.running, g.vm.spec = true, false, spec
	g.vm.built++
	return nil
}

func (g *fakeGuest) Container(context.Context) (driver.ComputerState, bool, error) {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	if !g.vm.exists {
		return driver.ComputerState{}, false, nil
	}
	st := driver.ComputerState{Generation: g.vm.spec.Generation, Power: driver.PowerAsleep}
	if g.vm.running {
		st.Power, st.Running = driver.PowerAwake, true
	}
	return st, true, nil
}

func (g *fakeGuest) Start(context.Context) error {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	if !g.vm.exists {
		return errors.New("no container")
	}
	g.vm.running = true
	return nil
}

func (g *fakeGuest) Stop(context.Context) error {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	g.vm.running = false
	return nil
}

func (g *fakeGuest) Exec(_ context.Context, user string, cmd []string, _ io.Reader) (int, string, error) {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	g.vm.execs = append(g.vm.execs, fmt.Sprint(user, cmd))
	return 0, "ok", nil
}

func (g *fakeGuest) Terminal(context.Context, uint, uint) (driver.Terminal, error) {
	return nil, errors.New("fake guest: no terminal")
}

func (g *fakeGuest) Sessions(context.Context) (int, error) {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	return g.vm.sessions, nil
}

func (g *fakeGuest) Events(ctx context.Context) <-chan driver.Event {
	out := make(chan driver.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-g.vm.events:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (g *fakeGuest) Close() error { return nil }

const (
	testProject       = "p"
	testZone          = "us-central1-a"
	controllerAccount = "a12s-controller@p.iam.gserviceaccount.com"
)

var controllerIP = netip.MustParseAddr("10.128.0.2")

// goodFirewall is what the fleet's bootstrap creates.
func goodFirewall() []gceapi.Firewall {
	return []gceapi.Firewall{
		{Name: "a12s-computers-deny-egress", Network: "net", Priority: 1000, Direction: "EGRESS", TargetTags: []string{NetworkTag},
			DestinationRanges: []string{"0.0.0.0/0"}, Denied: []gceapi.FirewallRule{{IPProtocol: "all"}}},
		{Name: "a12s-computers-to-controller", Network: "net", Priority: 900, Direction: "EGRESS", TargetTags: []string{NetworkTag},
			DestinationRanges: []string{"10.128.0.2/32"}, Allowed: []gceapi.FirewallRule{{IPProtocol: "tcp", Ports: []string{"3128", "7443"}}}},
		{Name: "a12s-controller-to-computers", Network: "net", Priority: 1000, Direction: "INGRESS", TargetTags: []string{NetworkTag},
			SourceRanges: []string{"10.128.0.2/32"}, Allowed: []gceapi.FirewallRule{{IPProtocol: "tcp", Ports: []string{"2376", "7443"}}}},
	}
}

type harness struct {
	t      *testing.T
	fake   *gcefake.Fake
	guests *fakeGuests
	d      *Driver
	ctx    context.Context
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	pollEvery = 0
	onLocalNetwork = func(netip.Prefix) (bool, error) { return true, nil }
	f := gcefake.New(testProject, testZone)
	t.Cleanup(f.Close)
	f.AddSubnetwork(gceapi.Subnetwork{Name: "a12s-lab", Network: "net", IPCidrRange: "10.128.0.0/20", Region: "us-central1"})
	f.SetFirewalls(goodFirewall())
	g := &fakeGuests{vms: map[string]*fakeVM{}}
	cfg := Config{
		Fleet: "lab", API: f.API(), Subnetwork: "a12s-lab", Controller: controllerIP, ControllerAccount: controllerAccount,
		StateDir: t.TempDir(), Local: engine.New(nil, "lab"), Guest: g.open, Arch: "amd64",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	d, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, fake: f, guests: g, d: d, ctx: context.Background()}
}

func (h *harness) must(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) state(name string) driver.ComputerState {
	h.t.Helper()
	obs, err := h.d.Observe(h.ctx)
	h.must(err)
	return obs.Computers[name]
}

func testSpec(name string, gen int64) driver.ComputerSpec {
	return driver.ComputerSpec{
		Name: name, Generation: gen, Image: "sha256:aaaaaaaaaaaaaaaa", Tier: "slim",
		Bundle:    driver.BundleRef{Kind: "codex", Version: "1.0.0", Image: "sha256:bbbbbbbbbbbbbbbb"},
		HomeDrive: driver.HomeDriveID(name),
	}
}

func (h *harness) stageNew(name string) driver.ComputerSpec {
	h.t.Helper()
	_, err := h.d.EnsureDrive(h.ctx, driver.DriveSpec{ID: driver.HomeDriveID(name), Kind: driver.DriveHome, Computer: name})
	h.must(err)
	spec := testSpec(name, 1)
	h.must(h.d.StageComputer(h.ctx, spec))
	return spec
}
