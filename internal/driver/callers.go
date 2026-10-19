package driver

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// CallerMap maps the addresses a substrate attributes egress connections by to Computer and Service names, refreshed
// from Snapshot. A refresh and a removal never interleave: once Remove has dropped a name, no snapshot taken before
// its removal finished can publish that name's address again, so a reused address never inherits another Computer's
// policy.
type CallerMap struct {
	Snapshot func(ctx context.Context) (map[netip.Addr]string, error)
	// Contended, if set (tests only), runs with the refresh's context when a refresh is about to wait for a removal.
	Contended func(ctx context.Context)

	refreshMu sync.Mutex // held by a refresh from snapshot to publish, and by a removal throughout
	mu        sync.Mutex
	m         map[netip.Addr]string
	at        time.Time
}

// staleAfter bounds how often a miss triggers a refresh, so a client probing addresses cannot hammer the substrate.
const staleAfter = time.Second

// Lookup answers from the last snapshot, refreshing on a miss when it is over a second old.
func (c *CallerMap) Lookup(addr netip.Addr) (string, bool) {
	c.mu.Lock()
	name, ok := c.m[addr]
	stale := time.Since(c.at) > staleAfter
	c.mu.Unlock()
	if ok || !stale {
		return name, ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Refresh(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	name, ok = c.m[addr]
	return name, ok
}

func (c *CallerMap) Refresh(ctx context.Context) error {
	if !c.refreshMu.TryLock() {
		if c.Contended != nil {
			c.Contended(ctx)
		}
		c.refreshMu.Lock()
	}
	defer c.refreshMu.Unlock()
	m, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m, c.at = m, time.Now()
	return nil
}

// Require refreshes and fails unless name has an address: a Computer whose egress would not be attributable must not
// start.
func (c *CallerMap) Require(ctx context.Context, name string) error {
	if err := c.Refresh(ctx); err != nil {
		return fmt.Errorf("egress identity: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.m {
		if n == name {
			return nil
		}
	}
	return fmt.Errorf("the Controller has no address that identifies %s's egress", name)
}

// Remove drops name's addresses and holds off refreshes until done is called, after the substrate object that owned
// the address is gone.
func (c *CallerMap) Remove(name string) (done func()) {
	c.refreshMu.Lock()
	c.mu.Lock()
	for a, n := range c.m {
		if n == name {
			delete(c.m, a)
		}
	}
	c.mu.Unlock()
	return c.refreshMu.Unlock
}
