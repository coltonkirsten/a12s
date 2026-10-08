package controller

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

func TestTypedCertificates(t *testing.T) {
	ca, _ := LoadCA(t.TempDir(), "lab")
	leaf, err := ca.ServiceCert("clock", 4)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := parseCert(leaf.CertPEM)
	if len(c.URIs) != 1 || c.URIs[0].String() != "a12s://lab/service/clock/4" || len(c.DNSNames) != 0 {
		t.Fatalf("SANs %v %v", c.URIs, c.DNSNames)
	}
	p, err := ca.PrincipalOf(c)
	if err != nil || p.String() != "service:clock" || p.Generation != 4 {
		t.Fatalf("%+v %v", p, err)
	}
	if leaf.Serial != c.SerialNumber.Text(16) {
		t.Fatal("serial")
	}
	other, _ := LoadCA(t.TempDir(), "other")
	ol, _ := other.ComputerCert("scout", 1)
	oc, _ := parseCert(ol.CertPEM)
	if _, err := ca.PrincipalOf(oc); err == nil {
		t.Fatal("another fleet's identity accepted")
	}
}

// mtlsClient is an HTTPS client presenting leaf to an MTLSHandler server.
func mtlsServer(t *testing.T, h *harness) *httptest.Server {
	t.Helper()
	serverTLS, err := h.c.ca.ServerTLS("a12s-lab-controller")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h.c.MTLSHandler(NewLeases(h.c.authorizeLease).Handler()))
	srv.TLS = serverTLS
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func mtlsDo(t *testing.T, srv *httptest.Server, ca *CA, leaf Leaf, method, path, body string) (int, api.Error) {
	t.Helper()
	cert, _ := leaf.TLS()
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: ca.Pool(), ServerName: "a12s-lab-controller"}}}
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	res, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var e api.Error
	_ = json.Unmarshal(b, &e)
	return res.StatusCode, e
}

func TestMTLSGrantsAndRegistry(t *testing.T) {
	h := newHarness(t)
	f := withServices(t, h.d, func(f *fleet.Fleet) {
		b := f.Computers["builder"]
		b.Grants = fleet.Grants{Send: []string{"reviewer"}}
		f.Computers["builder"] = b
	})
	h.up(f, nil, false)
	h.waitService("clock", api.PhaseReady)
	h.waitPhase("builder", api.PhaseReady)
	ctx := context.Background()

	// Each start registered a live certificate with its active grants.
	e, ok, _ := h.c.inbox.LiveCert(ctx, "service:clock")
	if !ok || len(e.Send) != 1 || e.Send[0] != "scout" || e.Generation != 1 {
		t.Fatalf("clock entry %+v %v", e, ok)
	}
	if e, ok, _ := h.c.inbox.LiveCert(ctx, "computer:builder"); !ok || e.Send[0] != "reviewer" {
		t.Fatalf("builder entry %+v", e)
	}
	srv := mtlsServer(t, h)

	// A certificate that is not the live one is refused, even well-formed.
	stray, _ := h.c.ca.ServiceCert("clock", 1)
	if code, e := mtlsDo(t, srv, h.c.ca, stray, "POST", "/v1/computers/scout/inbox", `{"body":"x"}`); code != http.StatusForbidden || !strings.Contains(e.Message, "not active") {
		t.Fatalf("stray cert: %d %+v", code, e)
	}
	// Register a known leaf as clock's live certificate (as a start does).
	leaf, _ := h.c.ca.ServiceCert("clock", 1)
	if err := h.c.registerCert(ctx, Principal{Kind: KindService, Name: "clock", Generation: 1}, leaf.Serial, []string{"scout"}); err != nil {
		t.Fatal(err)
	}
	if code, e := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/computers/scout/inbox", `{"body":"hello from the clock"}`); code != http.StatusCreated {
		t.Fatalf("granted send: %d %+v", code, e)
	}
	msgs, _ := h.c.inbox.List(ctx, "scout", inbox.ListOptions{State: "all"})
	if len(msgs.Messages) != 1 || msgs.Messages[0].Via != "service:clock" {
		t.Fatalf("messages %+v", msgs.Messages)
	}
	code, ae := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/computers/reviewer/inbox", `{"body":"x"}`)
	if code != http.StatusForbidden || ae.Code != api.ErrNoGrant || ae.Message != "clock may not send to reviewer" || ae.Fix != "add reviewer to services.clock.grants.send" {
		t.Fatalf("ungranted send: %d %+v", code, ae)
	}
	waitFor(t, func() bool {
		var b strings.Builder
		_ = h.c.writeServiceLogs(ctx, "clock", false, 0, func(s string) error { b.WriteString(s); return nil })
		return strings.Contains(b.String(), "denied: send to reviewer (no grant)")
	})
	for _, c := range []struct{ method, path string }{{"POST", "/v1/leases"}, {"PUT", "/v1/fleet"}, {"GET", "/v1/status"}} {
		if code, _ := mtlsDo(t, srv, h.c.ca, leaf, c.method, c.path, `{}`); code != http.StatusForbidden {
			t.Fatalf("%s %s by a Service: %d", c.method, c.path, code)
		}
	}
	// Sleep deletes the entry before the stop: the copied key stops working.
	if err := h.c.SetServicePower(ctx, "clock", api.ServicePowerRequest{State: api.PowerAsleep}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.c.inbox.LiveCert(ctx, "service:clock"); ok {
		t.Fatal("entry survived sleep")
	}
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "POST", "/v1/computers/scout/inbox", `{"body":"x"}`); code != http.StatusForbidden {
		t.Fatalf("after sleep: %d", code)
	}
}

func TestProxyInternalNames(t *testing.T) {
	h := newHarness(t)
	f := withServices(t, h.d, func(f *fleet.Fleet) {
		f.Services["db"] = fleet.Service{Image: "a12s/svc-test:0.1"}
		s := f.Computers["scout"]
		s.Uses = []fleet.Use{{Name: "db"}}
		f.Computers["scout"] = s
	})
	h.up(f, nil, false)
	h.waitService("db", api.PhaseReady)
	h.waitPhase("scout", api.PhaseReady)
	ctx := context.Background()
	if env := strings.Join(h.d.get("scout").spec.Env("lab"), " "); !strings.Contains(env, `"env":"A12S_SERVICE_DB_HTTP"`) {
		t.Fatalf("scout env lacks its wiring: %s", env)
	}
	target, status, reason := h.c.internalTarget(ctx, "scout", "db.a12s", 8080, false)
	if reason != "" || target != netip.MustParseAddrPort("10.213.200.2:8080") {
		t.Fatalf("scout -> db: %v %d %s", target, status, reason)
	}
	if _, _, reason := h.c.internalTarget(ctx, "scout", "db.a12s", 8080, true); reason != "" {
		t.Fatalf("plain http to an http port: %s", reason)
	}
	if _, status, reason := h.c.internalTarget(ctx, "reviewer", "db.a12s", 8080, false); status != http.StatusForbidden || reason != "reviewer cannot reach db: computers.reviewer.uses does not list db" {
		t.Fatalf("reviewer -> db: %d %s", status, reason)
	}
	if _, status, _ := h.c.internalTarget(ctx, "scout", "db.a12s", 9999, false); status != http.StatusForbidden {
		t.Fatalf("undeclared port: %d", status)
	}
	if _, status, _ := h.c.internalTarget(ctx, "scout", "clock.a12s", 8080, false); status != http.StatusForbidden {
		t.Fatalf("unused service: %d", status)
	}
	if err := h.c.SetServicePower(ctx, "db", api.ServicePowerRequest{State: api.PowerAsleep}); err != nil {
		t.Fatal(err)
	}
	if _, status, reason := h.c.internalTarget(ctx, "scout", "db.a12s", 8080, false); status != http.StatusServiceUnavailable || reason != "db is Asleep" {
		t.Fatalf("asleep: %d %s", status, reason)
	}
}

func TestEgressAddrDenied(t *testing.T) {
	pools := []netip.Prefix{netip.MustParsePrefix("10.213.0.0/16"), netip.MustParsePrefix("172.18.0.0/16")}
	cases := []struct {
		addr, want string
		allow      []string
	}{
		{"93.184.216.34", "", nil},
		{"10.0.5.5", "private", nil},
		{"10.0.5.5", "", []string{"10.0.0.0/8"}},
		{"10.213.0.2", "inside the Fleet's own networks", []string{"10.0.0.0/8"}},
		{"172.18.0.3", "inside the Fleet's own networks", []string{"172.16.0.0/12"}},
		{"127.0.0.1", "never reachable", []string{"127.0.0.0/8"}},
		{"169.254.169.254", "never reachable", []string{"169.254.0.0/16"}},
	}
	for _, c := range cases {
		got := egressAddrDenied(netip.MustParseAddr(c.addr), pools, c.allow)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s allow %v: %q, want %q", c.addr, c.allow, got, c.want)
		}
	}
}
