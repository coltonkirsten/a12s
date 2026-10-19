package driver

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// A refresh whose snapshot was taken before a removal never publishes it: the snapshot still shows builder at its
// old address, the removal finishes and tester takes the address, and only then is the stale snapshot handed back.
// The refresh sees that a removal happened and takes a new snapshot.
func TestStaleRefreshCannotOutliveRemoval(t *testing.T) {
	ctx := context.Background()
	src := &addrs{m: map[netip.Addr]string{ipB: "builder", ipT: "tester"}}
	var armed atomic.Bool
	stale, release := make(chan struct{}), make(chan struct{})
	c := &CallerMap{Snapshot: func(ctx context.Context) (map[netip.Addr]string, error) {
		m, err := src.snapshot(ctx)
		if armed.CompareAndSwap(true, false) {
			close(stale) // a snapshot showing builder: hold it
			<-release
		}
		return m, err
	}}
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	refreshed := make(chan error)
	go func() { refreshed <- c.Refresh(ctx) }()
	<-stale
	done := c.Remove("builder")
	src.set(netip.Addr{}, "builder")
	done()
	src.set(ipB, "tester")
	close(release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	if got, ok := c.Lookup(ipB); !ok || got != "tester" {
		t.Fatalf("Lookup(reused address) = %q, %v; want tester", got, ok)
	}
}

// A slow removal (a VM's deletion takes a minute) never holds up attributing other Computers' egress.
func TestRemovalDoesNotBlockRefresh(t *testing.T) {
	src := &addrs{m: map[netip.Addr]string{ipB: "builder", ipT: "tester"}}
	c := &CallerMap{Snapshot: src.snapshot}
	done := c.Remove("builder")
	defer done()
	finished := make(chan struct{})
	go func() {
		if got, ok := c.Lookup(ipT); !ok || got != "tester" {
			t.Errorf("Lookup(tester) = %q, %v", got, ok)
		}
		if _, ok := c.Lookup(ipB); ok {
			t.Error("a Computer being removed was attributed")
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Lookup waited for a removal")
	}
}
