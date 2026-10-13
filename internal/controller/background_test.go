package controller

import (
	"context"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// Background work keeps an agent computer Ready and deliverable, but not idle: sleepAfterIdle waits for it.
func TestBackgroundWorkKeepsItAwakeAndDeliverable(t *testing.T) {
	h := startHarnessScaled(t, t.TempDir(), nil, powerScale)
	scout := h.cds.get("scout")
	scout.set(func(f *fakeComputerd) { f.background = 1 })
	h.up(powerFleet(t, 6), false)
	time.Sleep(100 * time.Millisecond)
	if h.phase("scout") != api.PhaseReady {
		t.Fatalf("phase %s with background work running", h.phase("scout"))
	}
	if cs := h.c.computerStatus(context.Background(), "scout"); cs.Background == nil || cs.Background.Tasks != 1 {
		t.Fatalf("status background = %+v", cs.Background)
	}
	m := h.send("scout", "what is 17*3")
	h.waitMessage("scout", m.ID, api.MsgDelivered)
	scout.finish(api.TurnCompleted)
	time.Sleep(100 * time.Millisecond)
	if h.phase("scout") != api.PhaseReady {
		t.Fatal("slept while background work runs")
	}
	scout.task(false)
	h.waitPhase("scout", api.PhaseAsleep)
	if cs := h.c.computerStatus(context.Background(), "scout"); cs.Background != nil {
		t.Fatalf("background after sleep = %+v", cs.Background)
	}
}

// A Turn the harness starts by itself makes the agent computer Busy: Messages wait for it, then go at once.
func TestBackgroundTurnHoldsDelivery(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	scout := h.cds.get("scout")
	bg := scout.selfTurn()
	h.waitPhase("scout", api.PhaseBusy)
	m := h.send("scout", "hello")
	time.Sleep(50 * time.Millisecond)
	if h.message("scout", m.ID).State != api.MsgPending || len(scout.requestsSnapshot()) != 0 {
		t.Fatal("delivered during the background turn")
	}
	scout.finish(api.TurnCompleted)
	h.waitMessage("scout", m.ID, api.MsgDelivered)
	scout.finish(api.TurnCompleted)
	waitFor(t, func() bool {
		turns, err := h.c.inbox.Turns(context.Background(), "scout", 10)
		return err == nil && len(turns) == 2 && turns[1].ID == bg && turns[1].Reason == api.TurnReasonBackground && len(turns[1].Messages) == 0 && turns[0].Reason == ""
	})
}

// A change that restarts the agent computer waits for background work; a12s stop ends it, and the change applies.
func TestStagedChangeWaitsForBackgroundAndStopEndsIt(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	scout := h.cds.get("scout")
	scout.task(true)
	waitFor(t, func() bool { return h.c.computerStatus(context.Background(), "scout").Background != nil })
	res := h.up(labFleet(t, func(f *fleet.Fleet) {
		c := f.Computers["scout"]
		c.Harness.Model = "claude-fable-5-2"
		f.Computers["scout"] = c
	}), false)
	if len(res.Staged) != 1 {
		t.Fatalf("up with background work = %+v", res)
	}
	time.Sleep(50 * time.Millisecond)
	if readState(t, h.dir).Computers["scout"].Generation != 1 {
		t.Fatal("applied while background work runs")
	}
	stop := decodeBody[api.StopResponse](t, h.api("POST", "/v1/agent-computers/scout/stop", `{}`))
	if stop.Background != 1 || len(stop.Turns) != 0 {
		t.Fatalf("stop = %+v", stop)
	}
	waitFor(t, func() bool { return readState(t, h.dir).Computers["scout"].Generation == 2 })
}
