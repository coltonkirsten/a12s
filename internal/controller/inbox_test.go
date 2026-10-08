package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

func decodeBody[T any](t *testing.T, res *http.Response) T {
	t.Helper()
	defer res.Body.Close()
	var v T
	b, _ := io.ReadAll(res.Body)
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

func TestInboxAPI(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)

	res := h.api("POST", "/v1/computers/builder/inbox", `{"body":"add a health check","idempotencyKey":"k1"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("enqueue = %d %+v", res.StatusCode, apiError(t, res))
	}
	m := decodeBody[api.EnqueueResponse](t, res)
	if m.Seq != 1 || m.State != api.MsgPending || !strings.HasPrefix(m.ID, "m_") || len(m.ID) != 28 {
		t.Fatalf("enqueue response %+v", m)
	}
	// Idempotent repeat: 200 and the same id.
	res = h.api("POST", "/v1/computers/builder/inbox", `{"body":"add a health check","idempotencyKey":"k1"}`)
	if res.StatusCode != http.StatusOK || decodeBody[api.EnqueueResponse](t, res).ID != m.ID {
		t.Fatal("idempotent repeat")
	}
	if res := h.api("POST", "/v1/computers/nobody/inbox", `{"body":"x"}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown computer = %d", res.StatusCode)
	}
	res = h.api("POST", "/v1/computers/builder/inbox", `{"body":"x","expires":0}`)
	if e := apiError(t, res); res.StatusCode != http.StatusConflict || e.Code != api.ErrNotIdle || e.Details["reason"] == "" || e.Fix == "" {
		t.Fatalf("now or never = %d %+v", res.StatusCode, e)
	}

	list := decodeBody[api.InboxListResponse](t, h.api("GET", "/v1/computers/builder/inbox?bodies=true", ""))
	if len(list.Messages) != 1 || *list.Messages[0].Body != "add a health check" || list.Messages[0].Via != "local" || list.Limits.MaxMessages.Value != 200 || list.Limits.MaxMessages.Source != "builtin" {
		t.Fatalf("list %+v", list)
	}
	st := decodeBody[api.StatusResponse](t, h.api("GET", "/v1/status", ""))
	for _, cs := range st.Computers {
		if cs.Name == "builder" && (cs.Inbox == nil || cs.Inbox.Pending != 1) {
			t.Fatalf("status inbox %+v", cs.Inbox)
		}
	}

	if res := h.api("POST", "/v1/computers/builder/inbox/hold", `{}`); res.StatusCode != 200 || !decodeBody[api.InboxSummary](t, res).Held {
		t.Fatal("hold")
	}
	if res := h.api("POST", "/v1/computers/builder/inbox/resume", `{"session":"new"}`); res.StatusCode != 200 || decodeBody[api.InboxSummary](t, res).SessionOverride != "new" {
		t.Fatal("resume --new")
	}
	if res := h.api("DELETE", "/v1/computers/builder/inbox/"+m.ID, ""); res.StatusCode != 200 {
		t.Fatalf("withdraw = %d", res.StatusCode)
	}
	if res := h.api("DELETE", "/v1/computers/builder/inbox/"+m.ID, ""); apiError(t, res).Code != api.ErrWithdrawn {
		t.Fatal("second withdraw")
	}
	h.api("POST", "/v1/computers/builder/inbox", `{"body":"a"}`)
	h.api("POST", "/v1/computers/builder/inbox", `{"body":"b"}`)
	if c := decodeBody[api.ClearResponse](t, h.api("POST", "/v1/computers/builder/inbox/clear", `{}`)); c.Discarded != 2 {
		t.Fatalf("clear %+v", c)
	}
}

func TestScopedTokens(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)

	res := h.api("POST", "/v1/tokens", `{"name":"ui","actions":["send"],"computers":["builder"]}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %+v", res.StatusCode, apiError(t, res))
	}
	tok := decodeBody[api.Token](t, res)
	auth := []string{"Authorization", "Bearer " + tok.Secret}
	if res := h.api("POST", "/v1/tokens", `{"name":"Bad Name","actions":["send"],"computers":["*"]}`); res.StatusCode != 400 {
		t.Fatal("bad token name accepted")
	}

	res = h.api("POST", "/v1/computers/builder/inbox", `{"body":"from the ui"}`, auth...)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("send with token = %d", res.StatusCode)
	}
	mine := decodeBody[api.EnqueueResponse](t, res)
	// The owner's Message is invisible to the token; its own is not.
	owners := decodeBody[api.EnqueueResponse](t, h.api("POST", "/v1/computers/builder/inbox", `{"body":"owner"}`))
	if res := h.api("GET", "/v1/computers/builder/inbox/"+owners.ID, "", auth...); res.StatusCode != 404 {
		t.Fatalf("token read the owner's message: %d", res.StatusCode)
	}
	got := decodeBody[api.Message](t, h.api("GET", "/v1/computers/builder/inbox/"+mine.ID, "", auth...))
	if got.Via != "token:ui" || got.Sender != "token:ui" {
		t.Fatalf("via %+v", got)
	}
	if res := h.api("DELETE", "/v1/computers/builder/inbox/"+owners.ID, "", auth...); res.StatusCode != 404 {
		t.Fatal("token withdrew the owner's message")
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/computers/scout/inbox", `{"body":"x"}`},
		{"GET", "/v1/computers/builder/inbox", ""},
		{"POST", "/v1/computers/builder/inbox/clear", `{}`},
		{"PUT", "/v1/fleet", `{}`},
		{"GET", "/v1/drives", ""},
		{"POST", "/v1/tokens", `{"name":"x","actions":["inbox"],"computers":["*"]}`},
		{"POST", "/v1/computers/builder/power", `{"state":"asleep"}`},
		{"GET", "/v1/status", ""},
	} {
		res := h.api(c.method, c.path, c.body, auth...)
		if e := apiError(t, res); res.StatusCode != http.StatusForbidden || e.Fix == "" {
			t.Errorf("%s %s with a send token = %d %+v", c.method, c.path, res.StatusCode, e)
		}
	}
	if res := h.api("DELETE", "/v1/tokens/ui", ""); res.StatusCode != http.StatusNoContent {
		t.Fatal("revoke")
	}
	if res := h.api("POST", "/v1/computers/builder/inbox", `{"body":"x"}`, auth...); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d", res.StatusCode)
	}
}

// Gate 22 (S1 part): Fleet limits, admission, over-limit, survival across a restart.
func TestInboxFleetLimits(t *testing.T) {
	h := newHarness(t)
	five, three := int64(5), int64(3)
	f := labFleet(t, func(f *fleet.Fleet) {
		f.Defaults.Inbox.MaxMessages = &five
		for _, n := range f.ComputerNames() {
			c := f.Computers[n]
			c.Inbox.MaxMessages = &five
			if n == "builder" {
				c.Inbox.MaxMessages = &three
			}
			f.Computers[n] = c
		}
	})
	h.up(f, nil, false)
	for i := range 4 {
		res := h.api("POST", "/v1/computers/builder/inbox", `{"body":"x`+string(rune('a'+i))+`"}`)
		if i < 3 && res.StatusCode != 201 || i == 3 && res.StatusCode != 429 {
			t.Fatalf("builder send %d = %d", i, res.StatusCode)
		}
		if i == 3 && apiError(t, res).Fix == "" {
			t.Fatal("429 without fix")
		}
	}
	for i := range 5 {
		if res := h.api("POST", "/v1/computers/scout/inbox", `{"body":"x"}`); res.StatusCode != 201 {
			t.Fatalf("scout send %d = %d", i, res.StatusCode)
		}
	}
	l := decodeBody[api.InboxListResponse](t, h.api("GET", "/v1/computers/builder/inbox", ""))
	if l.Limits.MaxMessages.Value != 3 || l.Limits.MaxMessages.Source != "computer" {
		t.Fatalf("limits %+v", l.Limits)
	}
	ls := decodeBody[api.InboxListResponse](t, h.api("GET", "/v1/computers/scout/inbox", ""))
	if ls.Limits.MaxMessages.Source != "defaults" {
		t.Fatalf("scout limits %+v", ls.Limits)
	}
	// Lower builder to 1: nothing dropped, over limit, refused.
	one := int64(1)
	f2 := labFleet(t, func(f *fleet.Fleet) {
		c := f.Computers["builder"]
		c.Inbox.MaxMessages = &one
		f.Computers["builder"] = c
	})
	res := h.up(f2, nil, false)
	if len(res.Staged)+len(res.Updated) != 0 {
		t.Fatalf("a limit change restaged: %+v", res)
	}
	l = decodeBody[api.InboxListResponse](t, h.api("GET", "/v1/computers/builder/inbox", ""))
	if l.Pending != 3 || !l.OverLimit {
		t.Fatalf("after lowering %+v", l.InboxSummary)
	}
	if e := apiError(t, h.api("POST", "/v1/computers/builder/inbox", `{"body":"y"}`)); !strings.Contains(e.Fix, "over its limit") {
		t.Fatalf("over-limit fix %+v", e)
	}
	// Survives a Controller restart.
	h2 := h.restart()
	l = decodeBody[api.InboxListResponse](t, h2.api("GET", "/v1/computers/builder/inbox", ""))
	if l.Pending != 3 {
		t.Fatalf("after restart %+v", l.InboxSummary)
	}
}

func TestFleetInboxValidation(t *testing.T) {
	str := func(s string) *string { return &s }
	n := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		in   fleet.Inbox
		want string
	}{
		{fleet.Inbox{MaxBytes: str("2Mi")}, "above the 1Mi ceiling"},
		{fleet.Inbox{MaxBytes: str("4Ki")}, "below the 8Ki floor"},
		{fleet.Inbox{MaxBytes: str("lots")}, "is not a size"},
		{fleet.Inbox{MaxMessages: n(0)}, "out of range"},
		{fleet.Inbox{MaxMessages: n(2001)}, "out of range"},
		{fleet.Inbox{Intro: str(strings.Repeat("x", 513))}, "512-byte cap"},
		{fleet.Inbox{Intro: str(strings.Repeat("l\n", 8) + "l")}, "8-line cap"},
		{fleet.Inbox{Intro: str("bell\a")}, "control character"},
	} {
		f := labFleet(t, func(f *fleet.Fleet) {
			b := f.Computers["builder"]
			b.Inbox = c.in
			f.Computers["builder"] = b
		})
		err := f.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "fix:") {
			t.Errorf("%+v: %v", c.in, err)
		}
	}
	ok := labFleet(t, func(f *fleet.Fleet) {
		b := f.Computers["builder"]
		b.Inbox = fleet.Inbox{MaxBytes: str("768Ki"), Intro: str("Review requests.\nOne verdict each.")}
		f.Computers["builder"] = b
	})
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if w := ok.InboxWarnings(); len(w) != 1 || !strings.Contains(w[0], "long-context") {
		t.Fatalf("warnings %v", w)
	}
}

// Expiry runs on the Controller's timer, whatever the Computer is doing.
func TestInboxExpiryTimer(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, true)
	m := decodeBody[api.EnqueueResponse](t, h.api("POST", "/v1/computers/scout/inbox", `{"body":"soon","expires":1}`))
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := decodeBody[api.Message](t, h.api("GET", "/v1/computers/scout/inbox/"+m.ID, ""))
		if got.State == api.MsgExpired {
			if got.EndedAt.Sub(*got.ExpiresAt) > time.Second {
				t.Fatalf("expired late: %v after the deadline", got.EndedAt.Sub(*got.ExpiresAt))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("not expired: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
