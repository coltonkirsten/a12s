package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

// Gate 3: two sends to a sleeping Computer, then wake --clear-inbox: no Turn,
// two inbox.discarded records with bodies; a retry with the same requestId
// clears nothing that arrived later.
func TestWakeClearInbox(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, true)
	a := h.send("scout", "first")
	b := h.send("scout", "second")
	res := h.api("POST", "/v1/computers/scout/power", `{"state":"awake","clearInbox":true,"requestId":"req-1","credentials":{"anthropic":"e30="}}`)
	cs := decodeBody[api.ComputerStatus](t, res)
	if res.StatusCode != http.StatusOK || cs.InboxCleared == nil || *cs.InboxCleared != 2 {
		t.Fatalf("wake = %d %+v", res.StatusCode, cs)
	}
	scout := h.cds.get("scout")
	waitFor(t, func() bool { return len(scout.sealedSnapshot()) == 2 })
	sealed := scout.sealedSnapshot()
	if sealed[a.ID]["body"] != "first" || sealed[b.ID]["reason"] != "wake-clear" {
		t.Fatalf("sealed %+v", sealed)
	}
	if n := len(scout.requestsSnapshot()); n != 0 {
		t.Fatalf("a turn ran: %d", n)
	}
	// A message after the clear survives a retry of the same wake.
	h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAsleep})
	c := h.send("scout", "after")
	h.api("POST", "/v1/computers/scout/power", `{"state":"awake","clearInbox":true,"requestId":"req-1","credentials":{"anthropic":"e30="}}`).Body.Close()
	h.waitMessage("scout", c.ID, api.MsgDelivered)
}

// Gate 20 tail / §4.10: an expiry while asleep is journaled at wake; while
// awake (held), at once.
func TestDeferredJournaling(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, true)
	m := h.send("scout", "soon", `"expires":1`)
	h.waitMessage("scout", m.ID, api.MsgExpired)
	scout := h.cds.get("scout")
	if len(scout.sealedSnapshot()) != 0 {
		t.Fatal("journaled while asleep")
	}
	if err := h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAwake, Credentials: labCreds}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return scout.sealedSnapshot()[m.ID]["reason"] == "expired" })

	h.api("POST", "/v1/computers/scout/inbox/hold", `{}`).Body.Close()
	n := h.send("scout", "soon too", `"expires":1`)
	waitFor(t, func() bool { return scout.sealedSnapshot()[n.ID]["body"] == "soon too" })
}

// stop --hold holds before it interrupts: nothing is delivered after.
func TestStopHold(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	builder := h.cds.get("builder")
	h.send("builder", "long")
	h.waitPhase("builder", api.PhaseBusy)
	next := h.send("builder", "next")
	res := h.api("POST", "/v1/computers/builder/turns/t_1/stop?hold=true", `{}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stop --hold = %d", res.StatusCode)
	}
	h.waitPhase("builder", api.PhaseReady)
	time.Sleep(50 * time.Millisecond)
	if len(builder.requestsSnapshot()) != 1 || h.message("builder", next.ID).State != api.MsgPending {
		t.Fatal("delivered after stop --hold")
	}
}

// Gate 5 (second half): claimed rows, a hold and a session override survive
// a Controller restart and are acted on afterwards.
func TestInboxStateSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, true)
	h.send("builder", "claimed one")
	if b, _ := h.c.inbox.Claim(context.Background(), "builder"); b == nil {
		t.Fatal("no claim")
	}
	h.send("scout", "held one")
	h.api("POST", "/v1/computers/scout/inbox/hold", `{}`).Body.Close()
	h.api("POST", "/v1/computers/reviewer/inbox/resume", `{"session":"new"}`).Body.Close()
	h2 := h.restart()
	sum := func(name string) api.InboxSummary {
		return decodeBody[api.InboxSummary](t, h2.api("GET", "/v1/computers/"+name+"/inbox", ""))
	}
	if s := sum("builder"); s.Claimed != 1 {
		t.Fatalf("builder %+v", s)
	}
	if s := sum("scout"); !s.Held {
		t.Fatalf("scout %+v", s)
	}
	if s := sum("reviewer"); s.SessionOverride != "new" {
		t.Fatalf("reviewer %+v", s)
	}
}
