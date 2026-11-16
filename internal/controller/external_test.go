package controller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// externalTools serves an app outside the Fleet and returns the Service that names it, with an MCP port and an HTTP
// health check on the app's own port.
func externalTools(t *testing.T, healthy *atomic.Bool) (fleet.Service, int) {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(app.Close)
	host, ps, _ := net.SplitHostPort(app.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)
	return fleet.Service{External: &fleet.External{Host: host}, Manifest: &fleet.ServiceManifest{
		Ports:  map[string]fleet.ServicePort{"api": {Port: port, MCP: &fleet.PortMCP{Path: "/mcp"}}},
		Health: &fleet.ServiceHealth{HTTP: &fleet.HealthHTTP{Port: port, Path: "/healthz"}},
	}}, port
}

func TestExternalService(t *testing.T) {
	h := newHarness(t)
	var healthy atomic.Bool
	healthy.Store(true)
	tools, port := externalTools(t, &healthy)
	f := withServices(t, h.d, func(f *fleet.Fleet) {
		f.Services["tools"] = tools
		s := f.Computers["scout"]
		s.Uses = []fleet.Use{{Name: "tools"}}
		f.Computers["scout"] = s
	})
	res := h.up(f, false)
	if !slices.Contains(res.Services.Created, "tools") || len(res.Errors) > 0 {
		t.Fatalf("up: %+v", res)
	}
	h.waitService("tools", api.PhaseReady)
	h.waitPhase("scout", api.PhaseReady)
	if h.d.svc("tools") != nil {
		t.Fatal("a12s ran a container for an external Service")
	}
	if _, ok := h.pins().Services[fleet.ServiceKey("tools", "")]; ok {
		t.Fatal("an external Service has no image to pin")
	}

	// Its MCP port is a tool of the agent computer that uses it, like a container Service's.
	if env := strings.Join(h.d.get("scout").spec.Env("lab"), " "); !strings.Contains(env, `"tool":"a12s-tools"`) {
		t.Fatalf("scout env lacks the a12s-tools tool: %s", env)
	}

	// The proxy dials the external host on plain HTTP, only for those that use it, and refuses a tunnel.
	ctx := context.Background()
	host := tools.External.Host + ":" + strconv.Itoa(port)
	if target, status, reason := h.c.internalTarget(ctx, "scout", "tools.a12s", port, true); target != host || reason != "" {
		t.Fatalf("scout -> tools: %q %d %s", target, status, reason)
	}
	if _, status, reason := h.c.internalTarget(ctx, "reviewer", "tools.a12s", port, true); status != http.StatusForbidden || !strings.Contains(reason, "uses does not list tools") {
		t.Fatalf("reviewer -> tools: %d %s", status, reason)
	}
	if _, status, reason := h.c.internalTarget(ctx, "scout", "tools.a12s", port, false); status != http.StatusForbidden || !strings.Contains(reason, "plain http://tools.a12s:"+strconv.Itoa(port)) {
		t.Fatalf("CONNECT to tools: %d %s", status, reason)
	}

	st := h.c.serviceStatus("tools")
	if st.External != tools.External.Host || st.Pin != "" || st.EgressMode != "" || !slices.Equal(st.Ports, []string{"api:" + strconv.Itoa(port) + "/http"}) || !slices.Equal(st.UsedBy, []string{"scout"}) {
		t.Fatalf("status %+v", st)
	}

	// Health is checked at the external address: Unhealthy and back, never restarted.
	healthy.Store(false)
	h.waitService("tools", api.PhaseUnhealthy)
	healthy.Store(true)
	h.waitService("tools", api.PhaseReady)

	// No container: power and shell are refused with what to do; logs carry the audit lines.
	var he *httpError
	if err := h.c.SetServicePower(ctx, "tools", api.ServicePowerRequest{State: api.PowerAsleep}); !errors.As(err, &he) || he.status != http.StatusBadRequest || he.body.Fix == "" {
		t.Fatalf("sleep tools: %v", err)
	}
	if res := h.api("POST", "/v1/services/tools/shell", `{}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("shell into tools = %d", res.StatusCode)
	}
	var out strings.Builder
	if err := h.c.writeServiceLogs(ctx, "tools", false, 0, func(s string) error { out.WriteString(s); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"external at " + tools.External.Host, "Unhealthy: health: HTTP 500", "healthy again", "no container output: tools is external"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("logs lack %q:\n%s", want, out.String())
		}
	}

	// A Controller restart picks it up again.
	h = h.restart()
	h.waitService("tools", api.PhaseReady)

	// Removed from the Fleet: nothing to stop, its record goes.
	h.up(withServices(t, h.d, nil), false)
	if h.c.isService("tools") {
		t.Fatal("tools is still in the Fleet")
	}
	if _, status, _ := h.c.internalTarget(ctx, "scout", "tools.a12s", port, true); status != http.StatusForbidden {
		t.Fatalf("removed tools is reachable: %d", status)
	}
}

// A Service that becomes external loses the container a12s ran for it; with no health check it is Ready at once.
func TestServiceBecomesExternal(t *testing.T) {
	h := newHarness(t)
	h.up(withServices(t, h.d, nil), false)
	h.waitService("clock", api.PhaseReady)
	tools, _ := externalTools(t, new(atomic.Bool))
	tools.Manifest.Health = nil
	res := h.up(withServices(t, h.d, func(f *fleet.Fleet) { f.Services["clock"] = tools }), false)
	if !slices.Equal(res.Services.Updated, []string{"clock"}) || len(res.Errors) > 0 {
		t.Fatalf("up: %+v", res)
	}
	if h.c.servicePhase("clock") != api.PhaseReady {
		t.Fatalf("clock is %s", h.c.servicePhase("clock"))
	}
	if h.d.svc("clock") != nil {
		t.Fatal("the container stayed")
	}
}
