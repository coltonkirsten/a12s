package controller

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

// A Computer slept before `down` comes back asleep.
func TestSleptComputerStaysAsleepAcrossDown(t *testing.T) {
	h := newHarness(t)
	f := labFleet(t, nil)
	h.up(f, false)
	h.waitPhase("scout", api.PhaseReady)
	if err := h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAsleep}); err != nil {
		t.Fatal(err)
	}
	h.waitPhase("scout", api.PhaseAsleep)
	// down: the Controller and every container go, state.json too; fleet.db stays.
	h.stop()
	for _, n := range []string{"scout", "builder", "tester", "reviewer"} {
		_ = h.d.RemoveComputer(context.Background(), n)
	}
	if err := os.Remove(filepath.Join(h.dir, stateFile)); err != nil {
		t.Fatal(err)
	}
	h2 := startHarness(t, h.dir, h.d)
	h2.up(f, false)
	h2.waitPhase("builder", api.PhaseReady)
	h2.waitPhase("scout", api.PhaseAsleep)
}

// A certificate never outlives its container.
func TestCertificateRefusedWhileServiceIsDown(t *testing.T) {
	h := newHarness(t)
	h.up(withServices(t, h.d, nil), false)
	h.waitService("clock", api.PhaseReady)
	srv := mtlsServer(t, h)
	leaf, _ := h.c.ca.ServiceCert("clock", 1)
	ctx := context.Background()
	_ = h.c.registerCert(ctx, Principal{Kind: KindService, Name: "clock", Generation: 1}, leaf.Serial, []string{"scout"})
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/agent-computers/scout/inbox", `{"body":"x"}`); code != 201 {
		t.Fatalf("live: %d", code)
	}
	h.c.setServicePhase("clock", api.PhaseError, &api.Failure{Reason: "down"})
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/agent-computers/scout/inbox", `{"body":"x"}`); code != 403 {
		t.Fatalf("down: %d", code)
	}
}

// A revocation is recorded in memory first, so a
// registry delete that failed (the entry is still there) still refuses the
// certificate while the container runs; the next start clears it.
func TestRevocationHoldsWhenTheRegistryWriteFails(t *testing.T) {
	h := newHarness(t)
	h.up(withServices(t, h.d, nil), false)
	h.waitService("clock", api.PhaseReady)
	srv := mtlsServer(t, h)
	leaf, _ := h.c.ca.ServiceCert("clock", 1)
	ctx := context.Background()
	p := Principal{Kind: KindService, Name: "clock", Generation: 1}
	if err := h.c.registerCert(ctx, p, leaf.Serial, []string{"scout"}); err != nil {
		t.Fatal(err)
	}
	h.c.unregisterCert(KindService, "clock")
	// As if the delete had failed: the registry still holds the live entry.
	if err := h.c.inbox.RegisterCert(ctx, inbox.CertEntry{Principal: p.String(), Generation: 1, Serial: leaf.Serial, Send: []string{"scout"}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/agent-computers/scout/inbox", `{"body":"x"}`); code != http.StatusForbidden {
		t.Fatalf("revoked, registry write failed: %d", code)
	}
	if err := h.c.registerCert(ctx, p, leaf.Serial, []string{"scout"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/agent-computers/scout/inbox", `{"body":"x"}`); code != http.StatusCreated {
		t.Fatalf("after a fresh start: %d", code)
	}
}
