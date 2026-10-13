package controller

import (
	"context"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
)

// An expiry while asleep is journaled at wake; while
// awake (held), at once.
func TestDeferredJournaling(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), true)
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

	h.api("POST", "/v1/agent-computers/scout/inbox/hold", `{}`).Body.Close()
	n := h.send("scout", "soon too", `"expires":1`)
	waitFor(t, func() bool { return scout.sealedSnapshot()[n.ID]["body"] == "soon too" })
}

// Claimed rows, a hold and a session override survive
// a Controller restart and are acted on afterwards.
func TestInboxStateSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), true)
	h.send("builder", "claimed one")
	if b, _ := h.c.inbox.Claim(context.Background(), "builder"); b == nil {
		t.Fatal("no claim")
	}
	h.send("scout", "held one")
	h.api("POST", "/v1/agent-computers/scout/inbox/hold", `{}`).Body.Close()
	h.api("POST", "/v1/agent-computers/reviewer/inbox/resume", `{"session":"new"}`).Body.Close()
	h2 := h.restart()
	sum := func(name string) api.InboxSummary {
		return decodeBody[api.InboxSummary](t, h2.api("GET", "/v1/agent-computers/"+name+"/inbox", ""))
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
