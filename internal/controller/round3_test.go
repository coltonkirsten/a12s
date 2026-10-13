package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

func bumpModel(name string) func(*fleet.Fleet) {
	return func(f *fleet.Fleet) {
		c := f.Computers[name]
		c.Harness.Model = "bumped"
		f.Computers[name] = c
	}
}

// A running Computer whose computerd does not answer is not idle: a
// change is staged, not applied over a turn that may be running.
func TestUnknownStatusIsBusy(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	h.cds.get("builder").mute()
	h.d.takeCalls()
	out := h.up(labFleet(t, bumpModel("builder")), false)
	if !slices.Equal(out.Staged, []string{"builder"}) {
		t.Fatalf("up = %+v", out)
	}
	if calls := h.d.takeCalls(); hasCall(calls, "stage builder") || hasCall(calls, "power builder") {
		t.Fatalf("builder was restaged: %v", calls)
	}
}

// Ready is published only once Observe shows the generation running.
func TestReadyIsReadBack(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	ctx := context.Background()
	if err := h.c.SetPower(ctx, "scout", api.PowerRequest{State: api.PowerAsleep}); err != nil {
		t.Fatal(err)
	}
	h.d.setGhostStarts(true)
	err := h.c.SetPower(ctx, "scout", api.PowerRequest{State: api.PowerAwake, Credentials: labCreds})
	if err == nil || !strings.Contains(err.Error(), "running not observed") {
		t.Fatalf("wake = %v", err)
	}
	if h.phase("scout") != api.PhaseError {
		t.Fatalf("scout = %s, want Error", h.phase("scout"))
	}
}
