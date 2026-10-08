package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
)

// Astra final review #16: a Computer slept before `down` comes back asleep.
func TestSleptComputerStaysAsleepAcrossDown(t *testing.T) {
	h := newHarness(t)
	f := labFleet(t, nil)
	h.up(f, nil, false)
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
	h2.up(f, nil, false)
	h2.waitPhase("builder", api.PhaseReady)
	h2.waitPhase("scout", api.PhaseAsleep)
}

// Astra final review #3/#18: a certificate never outlives its container.
func TestCertificateRefusedWhileServiceIsDown(t *testing.T) {
	h := newHarness(t)
	h.up(withServices(t, h.d, nil), nil, false)
	h.waitService("clock", api.PhaseReady)
	srv := mtlsServer(t, h)
	leaf, _ := h.c.ca.ServiceCert("clock", 1)
	ctx := context.Background()
	_ = h.c.registerCert(ctx, Principal{Kind: KindService, Name: "clock", Generation: 1}, leaf.Serial, []string{"scout"})
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/computers/scout/inbox", `{"body":"x"}`); code != 201 {
		t.Fatalf("live: %d", code)
	}
	h.c.setServicePhase("clock", api.PhaseError, &api.Failure{Reason: "down"})
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/computers/scout/inbox", `{"body":"x"}`); code != 403 {
		t.Fatalf("down: %d", code)
	}
}
