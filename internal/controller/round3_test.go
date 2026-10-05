package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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

// #24: a running Computer whose computerd does not answer is not idle: a
// change is staged, not applied over a turn that may be running; only
// apply --interrupt forces it.
func TestUnknownStatusIsBusy(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	h.cds.get("builder").mute()
	h.d.takeCalls()
	out := h.up(labFleet(t, bumpModel("builder")), res.Lock, false)
	if !slices.Equal(out.Staged, []string{"builder"}) {
		t.Fatalf("up = %+v", out)
	}
	if calls := h.d.takeCalls(); hasCall(calls, "stage builder") || hasCall(calls, "power builder") {
		t.Fatalf("builder was restaged: %v", calls)
	}
	if r, err := h.c.Apply(context.Background(), api.ApplyRequest{}); err != nil || !slices.Equal(r.Pending, []string{"builder"}) {
		t.Fatalf("apply = %+v, %v", r, err)
	}
	r, err := h.c.Apply(context.Background(), api.ApplyRequest{Interrupt: true, Grace: api.Duration(10 * time.Millisecond)})
	if err != nil || !slices.Equal(r.Applied, []string{"builder"}) {
		t.Fatalf("apply --interrupt = %+v, %v", r, err)
	}
}

// #26: Ready is published only once Observe shows the generation running.
func TestReadyIsReadBack(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
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
