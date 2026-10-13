package controller

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

func (h *harness) api(method, path, body string, header ...string) *http.Response {
	h.t.Helper()
	srv := httptest.NewServer(h.c.Handler())
	h.t.Cleanup(srv.Close)
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token-0123456789")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func apiError(t *testing.T, res *http.Response) api.Error {
	t.Helper()
	defer res.Body.Close()
	var e api.Error
	if err := json.NewDecoder(res.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAPI(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)

	if res := h.api("GET", "/v1/status", "", "Authorization", "Bearer wrong"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", res.StatusCode)
	}
	res := h.api("GET", "/v1/status", "")
	var st api.StatusResponse
	json.NewDecoder(res.Body).Decode(&st)
	if res.StatusCode != 200 || len(st.Computers) != 4 || st.Computers[2].Name != "scout" || st.Computers[2].Session != "s_scout" || st.EgressEnforcement != "enforced" {
		t.Fatalf("status = %d %+v", res.StatusCode, st)
	}

	// A message is delivered as a turn; a second one waits in line.
	res = h.api("POST", "/v1/computers/scout/inbox", `{"body":"hi"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("enqueue = %d", res.StatusCode)
	}
	h.waitPhase("scout", api.PhaseBusy)
	if m := decodeBody[api.EnqueueResponse](t, h.api("POST", "/v1/computers/scout/inbox", `{"body":"again"}`)); m.State != api.MsgPending {
		t.Fatalf("second = %+v", m)
	}
	if e := apiError(t, h.api("POST", "/v1/computers/nobody/inbox", `{"body":"x"}`)); e.Code != api.ErrNotFound {
		t.Fatalf("unknown = %+v", e)
	}

	// Events are proxied as SSE, id per event.
	res = h.api("GET", "/v1/computers/scout/events", "")
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events content type %q", res.Header.Get("Content-Type"))
	}
	h.cds.get("scout").finish(api.TurnCompleted)
	r := bufio.NewReader(res.Body)
	id, _ := r.ReadString('\n')
	data, _ := r.ReadString('\n')
	res.Body.Close()
	if id != "id: 3\n" || !strings.Contains(data, `"type":"turn.finished"`) {
		t.Fatalf("event = %q %q", id, data)
	}

	// The shell request is journaled in the Computer.
	res = h.api("POST", "/v1/computers/builder/shell", `{"root":true}`)
	var sh api.ShellResponse
	json.NewDecoder(res.Body).Decode(&sh)
	if sh.Container != "a12s-lab-builder" || sh.User != "0:0" || len(h.cds.get("builder").diagnostics) != 1 {
		t.Fatalf("shell = %+v, diagnostics %v", sh, h.cds.get("builder").diagnostics)
	}

	// Sleep through the API; messages then wait.
	if res := h.api("POST", "/v1/computers/tester/power", `{"state":"asleep"}`); res.StatusCode != 200 {
		t.Fatalf("sleep = %d", res.StatusCode)
	}
	if m := decodeBody[api.EnqueueResponse](t, h.api("POST", "/v1/computers/tester/inbox", `{"body":"x"}`)); m.State != api.MsgPending || m.Phase != api.PhaseAsleep {
		t.Fatalf("asleep enqueue = %+v", m)
	}

	// Drives: an attached drive is refused; a detached one is destroyed.
	if e := apiError(t, h.api("DELETE", "/v1/drives/reference", "")); e.Code != api.ErrAttached {
		t.Fatalf("rm attached = %+v", e)
	}
	h.up(labFleet(t, func(f *fleet.Fleet) { delete(f.Computers, "tester") }), false)
	res = h.api("GET", "/v1/drives", "")
	var dr api.DrivesResponse
	json.NewDecoder(res.Body).Decode(&dr)
	var home api.DriveRow
	for _, d := range dr.Drives {
		if d.Name == "home-tester" {
			home = d
		}
	}
	if !home.Orphaned || home.Kind != api.DriveHome || home.Computer != "tester" {
		t.Fatalf("drives = %+v", dr.Drives)
	}
	if res := h.api("DELETE", "/v1/drives/home-tester", ""); res.StatusCode != http.StatusNoContent || h.d.hasDrive("home-tester") {
		t.Fatalf("rm home-tester = %d", res.StatusCode)
	}
}

func TestEgressDenialIsJournaled(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	p, ok := h.c.policy("scout")
	if !ok || p.Check("platform.claude.com", 443) != "" || p.Check("api.openai.com", 443) == "" {
		t.Fatalf("scout policy = %+v", p)
	}
	h.c.denied("scout", "evil.example", 443, "host is not in the egress allowlist")
	d := h.cds.get("scout").diagnostics
	if len(d) != 1 || !strings.HasPrefix(d[0], "egress.denied ") || !strings.Contains(d[0], `"host":"evil.example"`) {
		t.Fatalf("diagnostics = %v", d)
	}
}
