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

// #54: when two Computers' networks are removed and their replacements
// reuse the subnets in reverse, a connection is never attributed to the
// old owner of its address: the mapping is dropped with the network, and
// rebuilt from the engine before a replacement starts.
func TestRecycledSubnetsDoNotInheritIdentity(t *testing.T) {
	h := newHarness(t)
	h.c.selfAddrs = h.d.controllerAddrs
	h.up(labFleet(t, nil), nil, false)
	ctx := context.Background()
	for _, name := range lab4 {
		if got, ok := h.c.identify(h.d.ip(name)); !ok || got != name {
			t.Fatalf("identify(%s) = %q, %v", name, got, ok)
		}
	}
	ipB, ipT := h.d.ip("builder"), h.d.ip("tester")
	for _, name := range []string{"builder", "tester"} {
		obs, _, _ := h.c.observeOne(ctx, name)
		if err := h.c.release(ctx, name, obs); err != nil {
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

// #54: a Computer whose address cannot be verified does not start.
func TestStartFailsClosedWithoutAVerifiedAddress(t *testing.T) {
	h := newHarness(t)
	h.c.selfAddrs = h.d.controllerAddrs
	h.up(labFleet(t, nil), nil, false)
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

// #56: a refresh that starts while a Computer's network is being removed
// cannot snapshot that network and publish it after the address was reused
// and verified by another Computer. The refresh is launched mid-removal and
// held, if its snapshot still shows builder's network, until tester has
// taken builder's address and verified it; then it may publish.
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
	h.up(labFleet(t, nil), nil, false)
	ipB := h.d.ip("builder")

	refreshed := make(chan struct{})
	h.d.beforeRemove = func(name string) {
		if name != "builder" {
			return
		}
		armed.Store(true)
		go func() { h.c.refreshAddrs(ctx); close(refreshed) }()
		select { // give it every chance to snapshot during the removal
		case <-stale:
		case <-time.After(50 * time.Millisecond):
		}
	}
	obs, _, _ := h.c.observeOne(ctx, "builder")
	if err := h.c.release(ctx, "builder", obs); err != nil {
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

// #57: every refusal of an unmapped address is logged, including a cached
// miss within a second of the last refresh.
func TestUnknownAddressRefusalsAreLogged(t *testing.T) {
	h := newHarness(t)
	h.c.selfAddrs = h.d.controllerAddrs
	h.up(labFleet(t, nil), nil, false)
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
