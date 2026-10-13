package controller

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

// removeContainer stops a Computer and removes its container and network, as a restage does before it stages the
// new generation; the Computer stays in transition, so nothing restarts it.
func removeContainer(ctx context.Context, h *harness, name string) error {
	obs, _, err := h.c.observeOne(ctx, name)
	if err != nil {
		return err
	}
	h.c.setTransition(name, true)
	if err := h.c.stop(ctx, name, obs.Generation); err != nil {
		return err
	}
	return h.c.removeNetworked(ctx, name, "removal")
}

// When two Computers' networks are removed and their replacements
// reuse the subnets in reverse, a connection is never attributed to the
// old owner of its address: the mapping is dropped with the network, and
// rebuilt from the engine before a replacement starts.
func TestRecycledSubnetsDoNotInheritIdentity(t *testing.T) {
	h := newHarness(t)
	h.c.selfAddrs = h.d.controllerAddrs
	h.up(labFleet(t, nil), false)
	ctx := context.Background()
	for _, name := range lab4 {
		if got, ok := h.c.identify(h.d.ip(name)); !ok || got != name {
			t.Fatalf("identify(%s) = %q, %v", name, got, ok)
		}
	}
	ipB, ipT := h.d.ip("builder"), h.d.ip("tester")
	for _, name := range []string{"builder", "tester"} {
		if err := removeContainer(ctx, h, name); err != nil {
			t.Fatal(err)
		}
	}
	h.d.setIP("tester", ipB) // the replacements' networks, reversed
	h.d.setIP("builder", ipT)
	if got, ok := h.c.identify(ipB); ok && got != "tester" {
		t.Fatalf("builder's old address is still attributed to %s", got)
	}
	if err := h.c.verifyAddr(ctx, "tester"); err != nil { // what start does first
		t.Fatal(err)
	}
	if got, ok := h.c.identify(ipB); !ok || got != "tester" {
		t.Fatalf("identify(reused) = %q, %v", got, ok)
	}
	p, _ := h.c.policy("tester")
	if p.Check("staging.example.com", 443) != "" || p.Check("api.openai.com", 443) == "" {
		t.Fatalf("tester's connection got another policy: %+v", p)
	}
}

// A Computer whose address cannot be verified does not start.
func TestStartFailsClosedWithoutAVerifiedAddress(t *testing.T) {
	h := newHarness(t)
	h.c.selfAddrs = h.d.controllerAddrs
	h.up(labFleet(t, nil), false)
	ctx := context.Background()
	h.c.SetPower(ctx, "scout", api.PowerRequest{State: api.PowerAsleep})
	h.d.setAddrErr(errors.New("engine unavailable"))
	h.d.takeCalls()
	err := h.c.SetPower(ctx, "scout", api.PowerRequest{State: api.PowerAwake, Credentials: labCreds})
	if err == nil || !strings.Contains(err.Error(), "engine unavailable") {
		t.Fatalf("wake = %v", err)
	}
	if calls := h.d.takeCalls(); hasCall(calls, "power scout awake") {
		t.Fatalf("started without a verified address: %v", calls)
	}
}

// A refresh that starts while a Computer's network is being removed
// cannot snapshot that network and publish it after the address was reused
// and verified by another Computer. The refresh is launched mid-removal and
// held, if its snapshot still shows builder's network, until tester has
// taken builder's address and verified it; then it may publish. The removal
// waits until the refresh has either snapshotted or is blocked behind it:
// no timing window.
func TestStaleRefreshCannotOutliveRemoval(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var armed atomic.Bool
	stale, release := make(chan struct{}), make(chan struct{})
	h.c.selfAddrs = func(ctx context.Context) (map[string]netip.Addr, error) {
		m, err := h.d.controllerAddrs(ctx)
		if _, has := m["a12s-lab-builder"]; has && armed.CompareAndSwap(true, false) {
			close(stale) // a stale snapshot: hold it
			<-release
		}
		return m, err
	}
	h.up(labFleet(t, nil), false)
	ipB := h.d.ip("builder")

	type probe struct{}
	probeCtx := context.WithValue(ctx, probe{}, true)
	refreshed, contended := make(chan struct{}), make(chan struct{})
	h.c.netContended = func(ctx context.Context) {
		if ctx.Value(probe{}) != nil {
			close(contended)
		}
	}
	h.d.beforeRemove = func(name string) {
		if name != "builder" {
			return
		}
		armed.Store(true)
		go func() { h.c.refreshAddrs(probeCtx); close(refreshed) }()
		select { // the refresh either snapshots during the removal or waits for it
		case <-stale:
		case <-contended:
		}
	}
	if err := removeContainer(ctx, h, "builder"); err != nil {
		t.Fatal(err)
	}
	armed.Store(false)
	h.d.setIP("tester", ipB)                              // the address is reused...
	if err := h.c.verifyAddr(ctx, "tester"); err != nil { // ...and verified
		t.Fatal(err)
	}
	close(release)
	<-refreshed
	select {
	case <-stale:
		t.Error("a refresh snapshotted builder's network during its removal")
	default:
	}
	if got, ok := h.c.identify(ipB); !ok || got != "tester" {
		t.Fatalf("identify(reused address) = %q, %v; want tester", got, ok)
	}
}

// Every refusal of an unmapped address is logged, including a cached
// miss within a second of the last refresh.
func TestUnknownAddressRefusalsAreLogged(t *testing.T) {
	h := newHarness(t)
	h.c.selfAddrs = h.d.controllerAddrs
	h.up(labFleet(t, nil), false)
	unknown := netip.MustParseAddr("10.213.9.9")
	for range 2 {
		if _, ok := h.c.identify(unknown); ok {
			t.Fatal("unknown address identified")
		}
	}
	if n := strings.Count(h.log.String(), "refused a connection on 10.213.9.9"); n != 2 {
		t.Fatalf("logged %d refusals, want 2:\n%s", n, h.log.String())
	}
}

// Start's resume pass runs before any apply: an `up` arriving while the
// restarted Controller is still resuming waits for it; otherwise the resume
// would re-converge the Fleet behind it.
func TestApplyWaitsForTheResume(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	h.stop()
	gate := make(chan struct{})
	h.d.mu.Lock()
	h.d.observeGate = gate // the resume's first Observe
	h.d.mu.Unlock()
	h2 := startHarness(t, h.dir, h.d)
	// Held by the resume from the moment Start returned, not from whenever
	// its goroutine happens to be scheduled.
	if h2.c.applyMu.TryLock() {
		h2.c.applyMu.Unlock()
		close(gate)
		t.Fatal("Start returned with the apply lock free")
	}
	applied := make(chan struct{})
	go func() {
		h2.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(labFleet(t, nil)), Credentials: labCreds}, writeOpts{})
		close(applied)
	}()
	select {
	case <-applied:
		t.Fatal("an apply ran before the resume")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	<-applied
}
