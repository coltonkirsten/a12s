package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

func duringTurn(policy string) func(f *fleet.Fleet) {
	return func(f *fleet.Fleet) {
		b := f.Computers["builder"]
		b.Inbox.DuringTurn = &policy
		f.Computers["builder"] = b
	}
}

// steering starts builder with inbox.duringTurn: policy and a first Turn running and received.
func steering(t *testing.T, policy string) (*harness, *fakeComputerd, api.EnqueueResponse) {
	h := newHarness(t)
	h.up(labFleet(t, duringTurn(policy)), false)
	builder := h.cds.get("builder")
	builder.set(func(f *fakeComputerd) { f.steers = true })
	first := h.send("builder", "long task")
	h.waitMessage("builder", first.ID, api.MsgDelivered)
	h.waitPhase("builder", api.PhaseBusy)
	return h, builder, first
}

func inboxIDs(in *api.TurnInbox) []string {
	var out []string
	for _, m := range in.Messages {
		out = append(out, m.ID)
	}
	return out
}

// With steer, Messages sent during a Turn are handed to it in one Batch and delivered in that Turn.
func TestSteerJoinsRunningTurn(t *testing.T) {
	h, builder, _ := steering(t, fleet.InboxSteer)
	a := h.send("builder", "also check the logs", `"sender":"colton"`)
	waitFor(t, func() bool { return len(builder.deliveryReqsSnapshot()) == 1 })
	b := h.send("builder", "and the metrics")
	req := builder.deliveryReqsSnapshot()[0]
	if strings.Join(inboxIDs(req.Inbox), ",") != a.ID || req.RequestID != req.Inbox.Batch || !strings.Contains(req.Prompt, inbox.SteerIntro) {
		t.Fatalf("request %+v", req)
	}
	if m := h.message("builder", a.ID); m.State != api.MsgClaimed {
		t.Fatalf("a is %s while the harness has not taken it in", m.State)
	}
	builder.steerOutcome(api.InboxJoined)
	if m := h.waitMessage("builder", a.ID, api.MsgDelivered); m.Turn != "t_1" {
		t.Fatalf("a delivered in %s", m.Turn)
	}
	// The next Batch goes to the same Turn once the first settled.
	waitFor(t, func() bool { return len(builder.deliveryReqsSnapshot()) >= 2 })
	builder.steerOutcome(api.InboxJoined)
	if m := h.waitMessage("builder", b.ID, api.MsgDelivered); m.Turn != "t_1" {
		t.Fatalf("b delivered in %s", m.Turn)
	}
	if n := len(builder.requestsSnapshot()); n != 1 {
		t.Fatalf("%d Turns started", n)
	}
	builder.finish(api.TurnCompleted)
	waitFor(t, func() bool {
		return len(decodeBody[api.TurnsResponse](t, h.api("GET", "/v1/agent-computers/builder/turns", "")).Turns) == 1
	})
	tr := decodeBody[api.TurnsResponse](t, h.api("GET", "/v1/agent-computers/builder/turns", "")).Turns[0]
	if len(tr.Messages) != 3 || tr.Messages[0].Steered || !tr.Messages[1].Steered || tr.Messages[1].ID != a.ID || !tr.Messages[2].Steered {
		t.Fatalf("stored turn messages %+v", tr.Messages)
	}
}

// Without the policy (the default), nothing is handed to a running Turn, even by a harness that could take it.
func TestWaitKeepsMessagesForNextTurn(t *testing.T) {
	h, builder, _ := steering(t, fleet.InboxWait)
	a := h.send("builder", "later")
	time.Sleep(100 * time.Millisecond)
	if n := len(builder.deliveryReqsSnapshot()); n != 0 || h.message("builder", a.ID).State != api.MsgPending {
		t.Fatalf("handed to the running Turn: %d", n)
	}
	builder.finish(api.TurnCompleted)
	if m := h.waitMessage("builder", a.ID, api.MsgDelivered); m.Turn != "t_2" {
		t.Fatalf("delivered in %s", m.Turn)
	}
}

// The Turn ended before the harness took the Batch in: it runs as a Turn of its own and settles like any Batch.
func TestSteerTurnEndedFirst(t *testing.T) {
	h, builder, _ := steering(t, fleet.InboxSteer)
	a := h.send("builder", "one more thing")
	waitFor(t, func() bool { return len(builder.deliveryReqsSnapshot()) == 1 })
	builder.steerOutcome(api.InboxStarted)
	if m := h.waitMessage("builder", a.ID, api.MsgDelivered); m.Turn != "t_2" || m.Attempts != 0 {
		t.Fatalf("delivered %+v", m)
	}
	if n := len(builder.requestsSnapshot()); n != 1 {
		t.Fatalf("the Controller started a Turn too: %d", n)
	}
}

// A Batch the harness never received goes back to pending at its own seq without counting an attempt, and is
// delivered once, in the next Turn.
func TestSteerMissedReturnsWithoutAttempt(t *testing.T) {
	h, builder, _ := steering(t, fleet.InboxSteer)
	a := h.send("builder", "one more thing")
	waitFor(t, func() bool { return len(builder.deliveryReqsSnapshot()) == 1 })
	builder.set(func(f *fakeComputerd) { f.steers = false })
	builder.steerOutcome(api.InboxMissed)
	m := h.waitMessage("builder", a.ID, api.MsgPending)
	if m.Attempts != 0 {
		t.Fatalf("attempts %d", m.Attempts)
	}
	builder.finish(api.TurnCompleted)
	if m := h.waitMessage("builder", a.ID, api.MsgDelivered); m.Turn != "t_2" {
		t.Fatalf("delivered in %s", m.Turn)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(builder.requestsSnapshot()); n != 2 {
		t.Fatalf("delivered more than once: %d Turns", n)
	}
}

// A fresh Message needs a new session: it waits for the idle boundary, and so does what follows it.
func TestSteerFreshWaits(t *testing.T) {
	h, builder, _ := steering(t, fleet.InboxSteer)
	f := h.send("builder", "start over", `"fresh":true`)
	g := h.send("builder", "then this")
	time.Sleep(100 * time.Millisecond)
	if n := len(builder.deliveryReqsSnapshot()); n != 0 {
		t.Fatalf("handed to the running Turn: %d", n)
	}
	builder.finish(api.TurnCompleted)
	h.waitMessage("builder", g.ID, api.MsgDelivered)
	if req := builder.requestsSnapshot()[1]; req.Session != api.SessionNew || strings.Join(reqIDs(req), ",") != f.ID+","+g.ID {
		t.Fatalf("next Turn %+v", req)
	}
}

// The Controller dies between the POST and the settle: the replay reads the outcome and the Batch is delivered once.
func TestSteerControllerRestartBetweenPostAndSettle(t *testing.T) {
	h, builder, _ := steering(t, fleet.InboxSteer)
	a := h.send("builder", "one more thing")
	waitFor(t, func() bool { return len(builder.deliveryReqsSnapshot()) == 1 })
	h2 := h.restart()
	builder.steerOutcome(api.InboxJoined)
	if err := h2.c.SetPower(context.Background(), "builder", api.PowerRequest{State: api.PowerAwake, Credentials: labCreds}); err != nil {
		t.Fatal(err)
	}
	if m := h2.waitMessage("builder", a.ID, api.MsgDelivered); m.Turn != "t_1" {
		t.Fatalf("delivered in %s", m.Turn)
	}
	if n := len(builder.deliveryReqsSnapshot()); n != 1 {
		t.Fatalf("%d Batches handed over", n)
	}
}
