package controller

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

// Every refusal of an unattributable address is logged.
func TestUnknownAddressRefusalsAreLogged(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	if got, ok := h.c.identify(h.d.ip("scout"), netip.MustParseAddr("10.213.0.3")); !ok || got != "scout" {
		t.Fatalf("identify(scout) = %q, %v", got, ok)
	}
	unknown := netip.MustParseAddr("10.213.9.9")
	for range 2 {
		if _, ok := h.c.identify(unknown, netip.MustParseAddr("10.213.9.10")); ok {
			t.Fatal("unknown address identified")
		}
	}
	if n := strings.Count(h.log.String(), "on 10.213.9.9: no verified agent computer"); n != 2 {
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
