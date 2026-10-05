package controller

import (
	"context"
	"os"
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

// #30: the Fleet's egress enforcement is part of a Computer's generation:
// a busy Computer keeps its enforcement until its staged generation applies.
func TestEnforcementFollowsTheActiveGeneration(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	scout := h.cds.get("scout")
	scout.StartTurn(context.Background(), api.TurnRequest{RequestID: "r1", Prompt: "x"})
	h.waitPhase("scout", api.PhaseBusy)
	out := h.up(labFleet(t, func(f *fleet.Fleet) { f.Egress.Enforcement = fleet.EnforcementUnenforced }), res.Lock, false)
	if !slices.Equal(out.Staged, []string{"scout"}) || !slices.Equal(out.Updated, []string{"builder", "reviewer", "tester"}) {
		t.Fatalf("up = %+v", out)
	}
	if p, _ := h.c.policy("scout"); p.Check("anything.example", 443) == "" {
		t.Fatal("busy scout became unenforced before its generation applied")
	}
	if p, _ := h.c.policy("builder"); p.Check("anything.example", 443) != "" {
		t.Fatal("idle builder did not become unenforced")
	}
	if cs := h.c.computerStatus(context.Background(), "scout"); cs.EgressMode != api.EgressEnforced {
		t.Fatalf("scout mode = %q", cs.EgressMode)
	}
	scout.finish(api.TurnCompleted)
	waitFor(t, func() bool { p, _ := h.c.policy("scout"); return p.Check("anything.example", 443) == "" })
}

// #51: two idle Computers swapping exclusive drives converge: each releases
// the drive the other needs, and no drive ever has two attachments.
func TestExclusiveDriveSwap(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	swap := func(f *fleet.Fleet) {
		b, te := f.Computers["builder"], f.Computers["tester"]
		b.Drives, te.Drives = []fleet.DriveMount{{Name: "ux-scratch", At: "/work/ux"}}, []fleet.DriveMount{{Name: "a12s-repo", At: "/work/a12s"}}
		f.Computers["builder"], f.Computers["tester"] = b, te
	}
	h.up(labFleet(t, swap), res.Lock, false)
	t.Cleanup(func() {
		if t.Failed() {
			st := readState(t, h.dir)
			t.Logf("builder %+v\ntester %+v\nlog:\n%s", *st.Computers["builder"], *st.Computers["tester"], h.log.String())
		}
	})
	waitFor(t, func() bool {
		return slices.Equal(h.d.mounts("drive-ux-scratch"), []string{"builder"}) && slices.Equal(h.d.mounts("drive-a12s-repo"), []string{"tester"})
	})
	h.waitPhase("builder", api.PhaseReady)
	h.waitPhase("tester", api.PhaseReady)
	for _, name := range []string{"builder", "tester"} {
		if rec := readState(t, h.dir).Computers[name]; rec.pending() || rec.Generation < 2 {
			t.Errorf("%s: %+v", name, rec)
		}
	}
	if v := h.d.twoWriters(); len(v) > 0 {
		t.Fatalf("two writers: %v", v)
	}
}

// #52: a corrupted cached preset is repaired by the next up.
func TestCorruptPresetIsRepairedByUp(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	sum := res.Lock.Presets["./presets/scout"]
	if err := os.WriteFile(presetPath(h.dir, sum), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loadPreset(h.dir, sum) != nil {
		t.Fatal("corrupt entry loaded")
	}
	h.up(labFleet(t, nil), res.Lock, false)
	if loadPreset(h.dir, sum) == nil {
		t.Fatal("up did not repair the cached preset")
	}
}

// #53: a swap submitted while both holders are busy converges once both
// turns end, with no further command and never two attachments.
func TestExclusiveDriveSwapWhileBothBusy(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	builder, tester := h.cds.get("builder"), h.cds.get("tester")
	builder.StartTurn(context.Background(), api.TurnRequest{RequestID: "b", Prompt: "x"})
	tester.StartTurn(context.Background(), api.TurnRequest{RequestID: "t", Prompt: "x"})
	h.waitPhase("builder", api.PhaseBusy)
	h.waitPhase("tester", api.PhaseBusy)
	swap := func(f *fleet.Fleet) {
		b, te := f.Computers["builder"], f.Computers["tester"]
		b.Drives, te.Drives = []fleet.DriveMount{{Name: "ux-scratch", At: "/work/ux"}}, []fleet.DriveMount{{Name: "a12s-repo", At: "/work/a12s"}}
		f.Computers["builder"], f.Computers["tester"] = b, te
	}
	if out := h.up(labFleet(t, swap), res.Lock, false); !slices.Equal(out.Staged, []string{"builder", "tester"}) {
		t.Fatalf("up = %+v", out)
	}
	builder.finish(api.TurnCompleted)
	time.Sleep(30 * time.Millisecond) // builder releases a12s-repo and waits for ux-scratch
	tester.finish(api.TurnCompleted)
	waitFor(t, func() bool {
		return slices.Equal(h.d.mounts("drive-ux-scratch"), []string{"builder"}) && slices.Equal(h.d.mounts("drive-a12s-repo"), []string{"tester"})
	})
	h.waitPhase("builder", api.PhaseReady)
	h.waitPhase("tester", api.PhaseReady)
	if v := h.d.twoWriters(); len(v) > 0 {
		t.Fatalf("two writers: %v", v)
	}
}
