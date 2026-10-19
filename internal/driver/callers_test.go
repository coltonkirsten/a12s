package driver

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
)

type addrs struct {
	mu  sync.Mutex
	m   map[netip.Addr]string
	err error
}

func (a *addrs) snapshot(context.Context) (map[netip.Addr]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return nil, a.err
	}
	out := map[netip.Addr]string{}
	for k, v := range a.m {
		out[k] = v
	}
	return out, nil
}

func (a *addrs) set(ip netip.Addr, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range a.m {
		if v == name {
			delete(a.m, k)
		}
	}
	if name != "" {
		a.m[ip] = name
	}
}

var (
	ipB = netip.MustParseAddr("10.213.0.10")
	ipT = netip.MustParseAddr("10.213.0.18")
)

// When two Computers are removed and their replacements reuse the addresses in reverse, a connection is never
// attributed to the old owner of its address: the mapping goes with the removal, and Require rebuilds it before a
// replacement starts.
func TestRecycledAddressesDoNotInheritIdentity(t *testing.T) {
	ctx := context.Background()
	src := &addrs{m: map[netip.Addr]string{ipB: "builder", ipT: "tester"}}
	c := &CallerMap{Snapshot: src.snapshot}
	for ip, want := range map[netip.Addr]string{ipB: "builder", ipT: "tester"} {
		if got, ok := c.Lookup(ip); !ok || got != want {
			t.Fatalf("Lookup(%s) = %q, %v", ip, got, ok)
		}
	}
	for _, name := range []string{"builder", "tester"} {
		done := c.Remove(name)
		src.set(netip.Addr{}, name)
		done()
	}
	src.set(ipB, "tester")
	src.set(ipT, "builder")
	if got, ok := c.Lookup(ipB); ok && got != "tester" {
		t.Fatalf("builder's old address is still attributed to %s", got)
	}
	if err := c.Require(ctx, "tester"); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Lookup(ipB); !ok || got != "tester" {
		t.Fatalf("Lookup(reused) = %q, %v", got, ok)
	}
}

// A Computer whose address cannot be verified does not start.
func TestRequireFailsClosed(t *testing.T) {
	ctx := context.Background()
	src := &addrs{m: map[netip.Addr]string{ipB: "builder"}}
	c := &CallerMap{Snapshot: src.snapshot}
	if err := c.Require(ctx, "scout"); err == nil {
		t.Fatal("Require passed for a Computer with no address")
	}
	src.err = errors.New("engine unavailable")
	if err := c.Require(ctx, "builder"); err == nil {
		t.Fatal("Require passed without a snapshot")
	}
}

// A refresh that starts while a Computer is being removed cannot publish a snapshot showing it after the address was
// reused and verified by another Computer. The refresh is launched mid-removal and held, if its snapshot still shows
// builder, until tester has taken builder's address and verified it; then it may publish. The removal waits until
// the refresh has either snapshotted or is blocked behind it: no timing window.
func TestStaleRefreshCannotOutliveRemoval(t *testing.T) {
	ctx := context.Background()
	src := &addrs{m: map[netip.Addr]string{ipB: "builder", ipT: "tester"}}
	var armed atomic.Bool
	stale, release := make(chan struct{}), make(chan struct{})
	c := &CallerMap{Snapshot: func(ctx context.Context) (map[netip.Addr]string, error) {
		m, err := src.snapshot(ctx)
		if m[ipB] == "builder" && armed.CompareAndSwap(true, false) {
			close(stale)
			<-release
		}
		return m, err
	}}
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	type probe struct{}
	probeCtx := context.WithValue(ctx, probe{}, true)
	refreshed, contended := make(chan struct{}), make(chan struct{})
	c.Contended = func(ctx context.Context) {
		if ctx.Value(probe{}) != nil {
			close(contended)
		}
	}
	done := c.Remove("builder")
	armed.Store(true)
	go func() { c.Refresh(probeCtx); close(refreshed) }()
	select {
	case <-stale:
	case <-contended:
	}
	src.set(netip.Addr{}, "builder")
	done()
	armed.Store(false)
	src.set(ipB, "tester")
	if err := c.Require(ctx, "tester"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-refreshed
	select {
	case <-stale:
		t.Error("a refresh snapshotted builder during its removal")
	default:
	}
	if got, ok := c.Lookup(ipB); !ok || got != "tester" {
		t.Fatalf("Lookup(reused address) = %q, %v; want tester", got, ok)
	}
}
