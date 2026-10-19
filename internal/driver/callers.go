package driver

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// CallerMap maps the addresses a substrate attributes egress connections by to Computer and Service names, refreshed
// from Snapshot. A removal never waits for a refresh, nor a refresh for a removal, however slow either is. Instead a
// refresh publishes only if no removal began or ended while it took its snapshot, and never publishes a name being
// removed: once Remove has dropped a name, no snapshot taken before its removal finished can publish that name's
// address again, so a reused address never inherits another Computer's policy.
type CallerMap struct {
	Snapshot func(ctx context.Context) (map[netip.Addr]string, error)

	refreshMu sync.Mutex // one refresh at a time
	mu        sync.Mutex
	m         map[netip.Addr]string
	at        time.Time
	// epoch counts removals begun and ended; removing holds the names being removed.
	epoch    uint64
	removing map[string]int
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

var errRemovals = errors.New("removals kept changing the substrate during the refresh")

func (c *CallerMap) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	for range 5 {
		c.mu.Lock()
		start := c.epoch
		c.mu.Unlock()
		m, err := c.Snapshot(ctx)
		if err != nil {
			return err
		}
		c.mu.Lock()
		if c.epoch != start {
			c.mu.Unlock()
			continue // a removal began or ended during the snapshot: it may show a removed name
		}
		for a, n := range m {
			if c.removing[n] > 0 {
				delete(m, a)
			}
		}
		c.m, c.at = m, time.Now()
		c.mu.Unlock()
		return nil
	}
	return errRemovals
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

// Remove drops name's addresses and keeps them out of every refresh until done is called, after the substrate object
// that owned the address is gone.
func (c *CallerMap) Remove(name string) (done func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	if c.removing == nil {
		c.removing = map[string]int{}
	}
	c.removing[name]++
	for a, n := range c.m {
		if n == name {
			delete(c.m, a)
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.epoch++
			if c.removing[name]--; c.removing[name] == 0 {
				delete(c.removing, name)
			}
		})
	}
}
