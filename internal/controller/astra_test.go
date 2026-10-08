package controller

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/inbox"
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

// Astra final review #2: named egress fails closed until the Controller has
// verified its own bridge, then remembers the last verified one, so an
// engine read error never drops the bridge from the deny list.
func TestNamedEgressFailsClosedWithoutBridge(t *testing.T) {
	ctx := context.Background()
	engineErr := errors.New("engine unavailable")
	var addrs map[string]netip.Addr
	c := &Controller{cfg: Config{Fleet: "lab", SubnetPool: netip.MustParsePrefix("10.213.0.0/16")}}
	c.selfAddrs = func(context.Context) (map[string]netip.Addr, error) {
		if addrs == nil {
			return nil, engineErr
		}
		return addrs, nil
	}
	rc := reach{service: true, private: []string{"172.16.0.0/12"}, tcp: []fleet.TCPEgress{
		{Name: "pg", Host: "1.1.1.1", Port: 5432},
		{Name: "bridge", Host: "172.20.0.9", Port: 80},
	}}
	if _, code, why := c.egressTarget(ctx, "db", rc, "pg", 5432); code != http.StatusServiceUnavailable {
		t.Fatalf("no bridge verified: %d %s", code, why)
	}
	addrs = map[string]netip.Addr{"a12s-lab": netip.MustParseAddr("172.20.0.2")}
	if _, code, why := c.egressTarget(ctx, "db", rc, "pg", 5432); code != 0 {
		t.Fatalf("verified: %d %s", code, why)
	}
	addrs = nil // the engine read fails again: the last verified bridge still applies
	if _, code, why := c.egressTarget(ctx, "db", rc, "pg", 5432); code != 0 {
		t.Fatalf("remembered: %d %s", code, why)
	}
	// allowPrivate covers the bridge, but the Fleet's own bridge is never dialed.
	if _, code, why := c.egressTarget(ctx, "db", rc, "bridge", 80); code != http.StatusForbidden || !strings.Contains(why, "own networks") {
		t.Fatalf("bridge address: %d %s", code, why)
	}
}

// Astra final review #4: a revocation is recorded in memory first, so a
// registry delete that failed (the entry is still there) still refuses the
// certificate while the container runs; the next start clears it.
func TestRevocationHoldsWhenTheRegistryWriteFails(t *testing.T) {
	h := newHarness(t)
	h.up(withServices(t, h.d, nil), nil, false)
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
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/computers/scout/inbox", `{"body":"x"}`); code != http.StatusForbidden {
		t.Fatalf("revoked, registry write failed: %d", code)
	}
	if err := h.c.registerCert(ctx, p, leaf.Serial, []string{"scout"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/computers/scout/inbox", `{"body":"x"}`); code != http.StatusCreated {
		t.Fatalf("after a fresh start: %d", code)
	}
}
