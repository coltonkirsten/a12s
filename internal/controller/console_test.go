package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

// drain collects the data lines that arrive within d.
func drain(lines <-chan string, d time.Duration) []string {
	var out []string
	deadline := time.After(d)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return out
			}
			if strings.HasPrefix(l, "data: ") {
				out = append(out, l)
			}
		case <-deadline:
			return out
		}
	}
}

func countType(lines []string, typ, name string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, `"type":"`+typ+`"`) && (name == "" || strings.Contains(l, name)) {
			n++
		}
	}
	return n
}

func eventOf(t *testing.T, line string) api.Event {
	t.Helper()
	var ev api.Event
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	return ev
}

// Every inbox change reaches GET /v1/events as inbox.changed with the inbox's summary, a burst as one event, and a
// prefix token sees only its own agent computers' inboxes.
func TestInboxChangedEvent(t *testing.T) {
	h, biz := prefixHarness(t)
	owner := sse(t, h, "/v1/events", "test-token-0123456789", "")
	until(t, owner, "event: head")
	res := h.api("POST", "/v1/tokens", `{"name":"biz2","prefix":"biz-"}`)
	lane := sse(t, h, "/v1/events", decodeBody[api.Token](t, res).Secret, "")
	until(t, lane, "event: head")

	if r := h.api("POST", "/v1/agent-computers/scout/inbox/hold", `{}`); r.StatusCode != 200 {
		t.Fatalf("hold = %d", r.StatusCode)
	}
	got := until(t, owner, `"type":"inbox.changed"`)
	ev := eventOf(t, got[len(got)-1])
	var sum api.InboxSummary
	if err := json.Unmarshal(ev.Data, &sum); err != nil || ev.AgentComputer != "scout" || sum.Computer != "scout" || !sum.Held || sum.HoldReason != api.HoldOperator {
		t.Fatalf("inbox.changed = %s (%v)", got[len(got)-1], err)
	}

	// A burst of changes is one event.
	for range 20 {
		h.c.inboxChanged("scout")
	}
	if n := countType(drain(owner, 500*time.Millisecond), "inbox.changed", `"agentComputer":"scout"`); n != 1 {
		t.Fatalf("a burst made %d inbox.changed events", n)
	}
	if r := h.api("POST", "/v1/agent-computers/scout/inbox", `{"body":"hi"}`); r.StatusCode != 201 {
		t.Fatalf("send = %d", r.StatusCode)
	}
	until(t, owner, `"pending":1`)

	// The prefix token's lane sees its own inbox changes and none of scout's.
	if r := biz("PUT", "/v1/agent-computers/biz-a", bizSpec+"}"); r.StatusCode != 200 {
		t.Fatalf("create = %d", r.StatusCode)
	}
	if r := biz("POST", "/v1/agent-computers/biz-a/inbox/hold", `{}`); r.StatusCode != 200 {
		t.Fatalf("hold biz-a = %d", r.StatusCode)
	}
	seen := until(t, lane, `"type":"inbox.changed"`)
	for _, l := range append(seen, drain(lane, 300*time.Millisecond)...) {
		if strings.Contains(l, `"scout"`) {
			t.Fatalf("the prefix token saw %s", l)
		}
	}
}

// A Service's phase, health and restart count reach GET /v1/events as service.phase, named by the Service and not
// as an agent computer; a token outside the Service's name never sees it.
func TestServicePhaseEvent(t *testing.T) {
	h := newHarness(t)
	owner := sse(t, h, "/v1/events", "test-token-0123456789", "")
	until(t, owner, "event: head")
	h.up(withServices(t, h.d, nil), nil, false)
	h.waitService("clock", api.PhaseReady)
	got := until(t, owner, `"service":"clock","phase":"Ready"`)
	ev := eventOf(t, got[len(got)-1])
	var d api.ServicePhaseData
	if err := json.Unmarshal(ev.Data, &d); err != nil || ev.Type != api.EventServicePhase || ev.AgentComputer != "" || ev.Computer != "" ||
		d.Service != "clock" || d.Health != "ok" || d.Restarts != 0 {
		t.Fatalf("service.phase = %s (%v)", got[len(got)-1], err)
	}

	tok := func(body string) <-chan string {
		res := h.api("POST", "/v1/tokens", body)
		lines := sse(t, h, "/v1/events", decodeBody[api.Token](t, res).Secret, "")
		until(t, lines, "event: head")
		return lines
	}
	lane := tok(`{"name":"biz","prefix":"biz-"}`)
	scoutOnly := tok(`{"name":"scoutonly","actions":["events"],"computers":["scout"]}`)
	clockOnly := tok(`{"name":"clockonly","actions":["events"],"computers":["clock"]}`)

	h.d.killService("clock", 1)
	waitFor(t, func() bool { return h.c.serviceStatus("clock").Restarts == 1 })
	h.waitService("clock", api.PhaseReady)
	until(t, owner, `"restarts":1`)
	until(t, clockOnly, `"restarts":1`)
	for name, lines := range map[string]<-chan string{"prefix biz-": lane, "scout only": scoutOnly} {
		if n := countType(drain(lines, 300*time.Millisecond), "service.phase", ""); n != 0 {
			t.Fatalf("a %s token saw %d service.phase events", name, n)
		}
	}
	if n := countType(drain(owner, 100*time.Millisecond), "service.phase", ""); n > 2 {
		t.Fatalf("%d more service.phase events after Ready: one per change only", n)
	}
}

// Every update in a change list carries its leaves' values before and after, in a dry run, a write and the drift.
func TestChangeDiff(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	if r := h.api("PUT", "/v1/agent-computers/hello", helloSpec); r.StatusCode != 200 {
		t.Fatalf("create = %d", r.StatusCode)
	}
	patch := `{"instructions":"Be brief","grants":{"send":["scout"]}}`
	want := map[string][2]string{
		"grants.send":  {"", `["scout"]`},
		"instructions": {`"Be helpful"`, `"Be brief"`},
	}
	check := func(what string, ch api.Change) {
		t.Helper()
		b, _ := json.Marshal(ch)
		if ch.Op != api.ChangeUpdate || strings.Join(ch.Fields, ",") != "grants,instructions" || len(ch.Diff) != len(want) {
			t.Fatalf("%s: %s", what, b)
		}
		for _, d := range ch.Diff {
			w, ok := want[d.Path]
			if !ok || string(d.Before) != w[0] || string(d.After) != w[1] {
				t.Fatalf("%s: %s", what, b)
			}
		}
		if strings.Contains(string(b), `"before":null`) {
			t.Fatalf("%s: an absent side is sent: %s", what, b)
		}
	}
	dry := decodeBody[api.PutFleetResponse](t, h.api("PATCH", "/v1/agent-computers/hello?dryRun=true", patch))
	if !dry.DryRun || len(dry.Changes) != 1 {
		t.Fatalf("dry run = %+v", dry)
	}
	check("dry run", dry.Changes[0])
	res := decodeBody[api.PutFleetResponse](t, h.api("PATCH", "/v1/agent-computers/hello", patch))
	if len(res.Changes) != 1 {
		t.Fatalf("patch = %+v", res)
	}
	check("patch", res.Changes[0])

	if r := h.api("PATCH", "/v1/agent-computers/tester", `{"resources":{"cpu":4}}`); r.StatusCode != 200 {
		t.Fatalf("patch tester = %d", r.StatusCode)
	}
	for _, ch := range decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Drift {
		if ch.Name != "tester" {
			continue
		}
		if len(ch.Diff) != 1 || ch.Diff[0].Path != "resources.cpu" || string(ch.Diff[0].Before) != `"2"` || string(ch.Diff[0].After) != `"4"` {
			b, _ := json.Marshal(ch)
			t.Fatalf("drift = %s", b)
		}
		return
	}
	t.Fatal("no drift for tester")
}

// A credential's updatedAt moves when its value changes and survives a restart; a token's lastUsedAt is recorded
// when it authenticates.
func TestCredentialAndTokenTimes(t *testing.T) {
	dir := t.TempDir()
	persistCreds[dir] = true
	h := startHarness(t, dir, nil)
	h.up(labFleet(t, nil), nil, false)
	put := decodeBody[api.CredentialInfo](t, h.api("PUT", "/v1/credentials/hubspot", `{"value":"hs-1"}`))
	if put.UpdatedAt == nil || time.Since(*put.UpdatedAt) > time.Minute {
		t.Fatalf("put = %+v", put)
	}
	find := func(h *harness) api.CredentialInfo {
		for _, c := range decodeBody[api.CredentialsResponse](t, h.api("GET", "/v1/credentials", "")).Credentials {
			if c.Name == "hubspot" {
				return c
			}
		}
		t.Fatal("no hubspot")
		return api.CredentialInfo{}
	}
	if got := find(h); got.UpdatedAt == nil || !got.UpdatedAt.Equal(*put.UpdatedAt) {
		t.Fatalf("listed = %+v", got)
	}
	h2 := h.restart()
	if got := find(h2); got.UpdatedAt == nil || !got.UpdatedAt.Equal(*put.UpdatedAt) {
		t.Fatalf("after a restart = %+v", got)
	}

	tok := decodeBody[api.Token](t, h2.api("POST", "/v1/tokens", `{"name":"reader","actions":["status"],"computers":["*"]}`))
	lastUsed := func() *time.Time {
		for _, x := range decodeBody[api.TokensResponse](t, h2.api("GET", "/v1/tokens", "")).Tokens {
			if x.Name == "reader" {
				return x.LastUsedAt
			}
		}
		return nil
	}
	if lastUsed() != nil {
		t.Fatal("an unused token has lastUsedAt")
	}
	if r := h2.api("GET", "/v1/status", "", "Authorization", "Bearer "+tok.Secret); r.StatusCode != 200 {
		t.Fatalf("status = %d", r.StatusCode)
	}
	if lastUsed() == nil {
		t.Fatal("lastUsedAt not recorded")
	}
}
