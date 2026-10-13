package controller

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
)

const bizSpec = `{"harness":{"kind":"claude-code","version":"2.1.280"},"image":"slim","credentials":["anthropic"]`

// prefixHarness runs the lab Fleet and returns a call function for a prefix token on "biz-".
func prefixHarness(t *testing.T) (*harness, func(method, path, body string) *http.Response) {
	t.Helper()
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	res := h.api("POST", "/v1/tokens", `{"name":"bizapp","prefix":"biz-"}`)
	if res.StatusCode != 201 {
		t.Fatalf("create prefix token = %d", res.StatusCode)
	}
	tok := decodeBody[api.Token](t, res)
	if tok.Prefix != "biz-" || tok.Secret == "" {
		t.Fatalf("token = %+v", tok)
	}
	return h, func(method, path, body string) *http.Response {
		return h.api(method, path, body, "Authorization", "Bearer "+tok.Secret)
	}
}

func wantOutOfPrefix(t *testing.T, what string, res *http.Response) {
	t.Helper()
	if e := apiError(t, res); res.StatusCode != http.StatusForbidden || e.Code != api.ErrOutOfPrefix {
		t.Errorf("%s = %d %s (%s), want 403 out_of_prefix", what, res.StatusCode, e.Code, e.Message)
	}
}

func wantForbidden(t *testing.T, what string, res *http.Response) {
	t.Helper()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("%s = %d, want 403", what, res.StatusCode)
	}
	res.Body.Close()
}

// Inside its prefix the token is the owner: create, send, power, credentials, remove.
func TestPrefixTokenOwnsItsLane(t *testing.T) {
	h, biz := prefixHarness(t)
	if r := biz("PUT", "/v1/agent-computers/biz-a", bizSpec+`,"instructions":"sell"}`); r.StatusCode != 200 {
		t.Fatalf("create inside = %d %+v", r.StatusCode, apiError(t, r))
	}
	if r := biz("POST", "/v1/agent-computers/biz-a/inbox", `{"body":"hi"}`); r.StatusCode != 201 {
		t.Fatalf("send inside = %d", r.StatusCode)
	}
	if r := biz("PATCH", "/v1/agent-computers/biz-a", `{"egress":{"allow":["*"]}}`); r.StatusCode != 200 {
		t.Fatalf("patch inside = %d (egress is its call)", r.StatusCode)
	}
	if r := biz("PUT", "/v1/credentials/biz-key", `{"value":"v"}`); r.StatusCode != 200 {
		t.Fatalf("set own credential = %d", r.StatusCode)
	}
	if r := biz("POST", "/v1/agent-computers/biz-a/power", `{"state":"asleep"}`); r.StatusCode != 200 {
		t.Fatalf("sleep inside = %d", r.StatusCode)
	}
	if r := biz("DELETE", "/v1/agent-computers/biz-a", ""); r.StatusCode != 200 {
		t.Fatalf("remove inside = %d", r.StatusCode)
	}
	if h.d.get("scout").spec.Name == "" {
		t.Fatal("scout is untouched")
	}
}

// Nothing outside the prefix can be created, renamed into, changed, removed or reached, by any path.
func TestPrefixTokenCannotLeaveItsLane(t *testing.T) {
	h, biz := prefixHarness(t)
	v0 := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Version

	// Renaming out of the prefix: there is no rename, so it is a create outside, by the new or the old path.
	wantOutOfPrefix(t, "create sales", biz("PUT", "/v1/agent-computers/sales", bizSpec+"}"))
	wantOutOfPrefix(t, "create sales (old path)", biz("PUT", "/v1/agent-computers/sales", bizSpec+"}"))
	wantOutOfPrefix(t, "create a dry run outside", biz("PUT", "/v1/agent-computers/sales?dryRun=true", bizSpec+"}"))
	wantOutOfPrefix(t, "patch scout", biz("PATCH", "/v1/agent-computers/scout", `{"instructions":"mine now"}`))
	wantOutOfPrefix(t, "delete scout", biz("DELETE", "/v1/agent-computers/scout", ""))
	wantOutOfPrefix(t, "create service", biz("PUT", "/v1/services/clock", `{"image":"x/y:1"}`))
	wantForbidden(t, "whole-Fleet write", biz("PUT", "/v1/fleet", `{"fleet":{"apiVersion":"a12s.ai/v1alpha1","kind":"Fleet","metadata":{"name":"lab"},"agent_computers":{}}}`))
	wantForbidden(t, "mint a token", biz("POST", "/v1/tokens", `{"name":"more","prefix":"biz-"}`))
	wantForbidden(t, "read the Fleet", biz("GET", "/v1/fleet", ""))
	wantForbidden(t, "remove a drive", biz("DELETE", "/v1/drives/home-scout", ""))

	// Send, uses and grants pointing outside.
	wantOutOfPrefix(t, "send to scout", biz("POST", "/v1/agent-computers/scout/inbox", `{"body":"x"}`))
	wantOutOfPrefix(t, "grant send to scout", biz("PUT", "/v1/agent-computers/biz-b", bizSpec+`,"grants":{"send":["scout"]}}`))
	wantOutOfPrefix(t, "use a Service outside", biz("PUT", "/v1/agent-computers/biz-b", bizSpec+`,"uses":["db"]}`))
	wantOutOfPrefix(t, "a Service granting outside", biz("PUT", "/v1/services/biz-clock", `{"image":"x/y:1","grants":{"send":["scout"]}}`))
	wantOutOfPrefix(t, "set a credential outside", biz("PUT", "/v1/credentials/anthropic", `{"value":"stolen"}`))
	wantOutOfPrefix(t, "remove a credential outside", biz("DELETE", "/v1/credentials/github", ""))
	wantOutOfPrefix(t, "stop scout", biz("POST", "/v1/agent-computers/scout/turns/t_1/stop", ""))
	wantOutOfPrefix(t, "sleep scout", biz("POST", "/v1/agent-computers/scout/power", `{"state":"asleep"}`))
	wantOutOfPrefix(t, "shell into scout", biz("POST", "/v1/agent-computers/scout/shell", `{}`))

	if v := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Version; v != v0 {
		t.Fatalf("refused writes changed the Fleet: v%d -> v%d", v0, v)
	}
}

// A seccomp profile loosens the sandbox, so only the owner sets one: a prefix token can't add, change or remove an
// agent computer's own security.seccomp, by any write path, though it may edit everything else of one the owner set.
func TestPrefixTokenCannotSetSeccomp(t *testing.T) {
	h, biz := prefixHarness(t)
	const profile = `{"defaultAction":"SCMP_ACT_ALLOW"}`
	const other = `{"defaultAction":"SCMP_ACT_ALLOW","syscalls":[{"names":["ptrace"],"action":"SCMP_ACT_ERRNO"}]}`
	wantSeccompForbidden := func(what string, res *http.Response) {
		t.Helper()
		if e := apiError(t, res); res.StatusCode != http.StatusForbidden || e.Code != api.ErrForbidden || !strings.Contains(e.Message, "seccomp") {
			t.Errorf("%s = %d %s (%s), want 403 forbidden naming seccomp", what, res.StatusCode, e.Code, e.Message)
		}
	}
	wantSeccompForbidden("create with a profile", biz("PUT", "/v1/agent-computers/biz-a", bizSpec+`,"security":{"seccomp":`+profile+`}}`))
	wantSeccompForbidden("dry-run create with a profile", biz("PUT", "/v1/agent-computers/biz-a?dryRun=true", bizSpec+`,"security":{"seccomp":`+profile+`}}`))
	if r := biz("PUT", "/v1/agent-computers/biz-a", bizSpec+"}"); r.StatusCode != 200 {
		t.Fatalf("create without a profile = %d", r.StatusCode)
	}
	wantSeccompForbidden("patch a profile in", biz("PATCH", "/v1/agent-computers/biz-a", `{"security":{"seccomp":`+profile+`}}`))

	if r := h.api("PUT", "/v1/agent-computers/biz-s", bizSpec+`,"security":{"seccomp":`+profile+`}}`); r.StatusCode != 200 {
		t.Fatalf("owner creates biz-s with a profile = %d %+v", r.StatusCode, apiError(t, r))
	}
	v := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Version
	wantSeccompForbidden("patch the owner's profile", biz("PATCH", "/v1/agent-computers/biz-s", `{"security":{"seccomp":`+other+`}}`))
	wantSeccompForbidden("patch the owner's profile away", biz("PATCH", "/v1/agent-computers/biz-s", `{"security":null}`))
	wantSeccompForbidden("put without the owner's profile", biz("PUT", "/v1/agent-computers/biz-s", bizSpec+"}"))
	if got := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Version; got != v {
		t.Fatalf("refused writes changed the Fleet: v%d -> v%d", v, got)
	}
	if r := biz("PATCH", "/v1/agent-computers/biz-s", `{"instructions":"sell"}`); r.StatusCode != 200 {
		t.Fatalf("patch other fields of biz-s = %d %+v", r.StatusCode, apiError(t, r))
	}
	if r := biz("PUT", "/v1/agent-computers/biz-s", bizSpec+`,"security":{"seccomp":`+profile+`}}`); r.StatusCode != 200 {
		t.Fatalf("put keeping the owner's profile = %d %+v", r.StatusCode, apiError(t, r))
	}
	if r := biz("DELETE", "/v1/agent-computers/biz-s", ""); r.StatusCode != 200 {
		t.Fatalf("remove biz-s = %d", r.StatusCode)
	}
}

// Defaults are merged into the token's agent computers, so they can't carry it outside either.
func TestPrefixTokenCannotReachOutThroughDefaults(t *testing.T) {
	h, biz := prefixHarness(t)
	if r := biz("PUT", "/v1/agent-computers/biz-a", bizSpec+"}"); r.StatusCode != 200 {
		t.Fatalf("create = %d", r.StatusCode)
	}
	fr := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", ""))
	fr.Fleet.Defaults = json.RawMessage(`{"resources":{"cpu":1,"memory":"2Gi"},"egress":{"allow":[]},"grants":{"send":["scout"]}}`)
	body, _ := json.Marshal(api.PutFleetRequest{Fleet: &fr.Fleet})
	if r := h.api("PUT", "/v1/fleet", string(body)); r.StatusCode != 200 {
		t.Fatalf("owner sets defaults = %d %+v", r.StatusCode, apiError(t, r))
	}
	res := biz("PUT", "/v1/agent-computers/biz-b", bizSpec+"}")
	e := apiError(t, res)
	if res.StatusCode != 403 || e.Code != api.ErrOutOfPrefix || !strings.Contains(e.Message, "defaults") {
		t.Fatalf("a new agent computer inheriting grants to scout = %d %s %s", res.StatusCode, e.Code, e.Message)
	}
	wantOutOfPrefix(t, "patch an existing one", biz("PATCH", "/v1/agent-computers/biz-a", `{"instructions":"x"}`))
	if r := biz("PUT", "/v1/agent-computers/biz-b", bizSpec+`,"grants":{"send":[]}}`); r.StatusCode != 200 {
		t.Fatalf("overriding the defaults' grants with its own = %d", r.StatusCode)
	}
}

// Reads show only names inside the prefix: status, lists, events, inboxes, Services and credentials.
func TestPrefixTokenReadsOnlyItsLane(t *testing.T) {
	_, biz := prefixHarness(t)
	if r := biz("PUT", "/v1/agent-computers/biz-a", bizSpec+"}"); r.StatusCode != 200 {
		t.Fatalf("create = %d", r.StatusCode)
	}
	st := decodeBody[api.StatusResponse](t, biz("GET", "/v1/status", ""))
	if len(st.AgentComputers) != 1 || st.AgentComputers[0].Name != "biz-a" || len(st.Warnings) != 0 || len(st.OrphanedDrives) != 0 {
		t.Fatalf("status = %+v", st)
	}
	list := decodeBody[api.AgentComputersResponse](t, biz("GET", "/v1/agent-computers", ""))
	if len(list.AgentComputers) != 1 || list.AgentComputers[0].Name != "biz-a" {
		t.Fatalf("list = %+v", list)
	}
	creds := decodeBody[api.CredentialsResponse](t, biz("GET", "/v1/credentials", ""))
	for _, c := range creds.Credentials {
		if !strings.HasPrefix(c.Name, "biz-") {
			t.Errorf("credential %s listed", c.Name)
		}
	}
	if svcs := decodeBody[api.ServicesResponse](t, biz("GET", "/v1/services", "")); len(svcs.Services) != 0 {
		t.Errorf("services = %+v", svcs)
	}
	wantOutOfPrefix(t, "get scout", biz("GET", "/v1/agent-computers/scout", ""))
	wantOutOfPrefix(t, "scout's events", biz("GET", "/v1/agent-computers/scout/events", ""))
	wantOutOfPrefix(t, "scout's inbox", biz("GET", "/v1/agent-computers/scout/inbox", ""))
	wantOutOfPrefix(t, "a scout message", biz("GET", "/v1/agent-computers/scout/inbox/m_1", ""))
	wantOutOfPrefix(t, "a Service outside", biz("GET", "/v1/services/db", ""))
	wantForbidden(t, "the token list", biz("GET", "/v1/tokens", ""))
}
