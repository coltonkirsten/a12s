package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
)

// #54: when two Computers' networks are removed and their replacements
// reuse the subnets in reverse, a connection is never attributed to the
// old owner of its address: the mapping is dropped with the network, and
// rebuilt from the engine before a replacement starts.
func TestRecycledSubnetsDoNotInheritIdentity(t *testing.T) {
	h := newHarness(t)
	h.c.selfAddrs = h.d.controllerAddrs
	h.up(labFleet(t, nil), nil, false)
	ctx := context.Background()
	for _, name := range lab4 {
		if got, ok := h.c.identify(h.d.ip(name)); !ok || got != name {
			t.Fatalf("identify(%s) = %q, %v", name, got, ok)
		}
	}
	ipB, ipT := h.d.ip("builder"), h.d.ip("tester")
	for _, name := range []string{"builder", "tester"} {
		obs, _, _ := h.c.observeOne(ctx, name)
		if err := h.c.release(ctx, name, obs); err != nil {
			t.Fatal(err)
		}
	}
	h.d.setIP("tester", ipB) // the replacements' networks, reversed
	h.d.setIP("builder", ipT)
	if got, ok := h.c.identify(ipB); ok && got != "tester" {
		t.Fatalf("builder's old address is still attributed to %s", got)
	}
	if err := h.c.verifyAddr(ctx, "tester"); err != nil { // what start does first
		t.Fatal(err)
	}
	if got, ok := h.c.identify(ipB); !ok || got != "tester" {
		t.Fatalf("identify(reused) = %q, %v", got, ok)
	}
	p, _ := h.c.policy("tester")
	if p.Check("staging.example.com", 443) != "" || p.Check("api.openai.com", 443) == "" {
		t.Fatalf("tester's connection got another policy: %+v", p)
	}
}

// #54: a Computer whose address cannot be verified does not start.
func TestStartFailsClosedWithoutAVerifiedAddress(t *testing.T) {
	h := newHarness(t)
	h.c.selfAddrs = h.d.controllerAddrs
	h.up(labFleet(t, nil), nil, false)
	ctx := context.Background()
	h.c.SetPower(ctx, "scout", api.PowerRequest{State: api.PowerAsleep})
	h.d.setAddrErr(errors.New("engine unavailable"))
	h.d.takeCalls()
	err := h.c.SetPower(ctx, "scout", api.PowerRequest{State: api.PowerAwake, Credentials: labCreds})
	if err == nil || !strings.Contains(err.Error(), "engine unavailable") {
		t.Fatalf("wake = %v", err)
	}
	if calls := h.d.takeCalls(); hasCall(calls, "power scout awake") {
		t.Fatalf("started without a verified address: %v", calls)
	}
}
