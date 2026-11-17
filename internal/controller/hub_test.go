package controller

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

// sse opens a stream and returns its data lines (and event: lines) as they arrive.
func sse(t *testing.T, h *harness, path, token, lastID string) <-chan string {
	t.Helper()
	srv := httptest.NewServer(h.c.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); srv.Close() })
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("GET %s: %v %v", path, res, err)
	}
	out := make(chan string, 1000)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") || strings.HasPrefix(l, "event: ") || strings.HasPrefix(l, "id: ") {
				out <- l
			}
		}
	}()
	return out
}

// until reads lines until one contains want; it fails after a few seconds.
func until(t *testing.T, lines <-chan string, want string) []string {
	t.Helper()
	var seen []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended before %q; saw %q", want, seen)
			}
			seen = append(seen, l)
			if strings.Contains(l, want) {
				return seen
			}
		case <-deadline:
			t.Fatalf("no %q; saw %q", want, seen)
		}
	}
}

// One connection carries every agent computer's events, version changes and phases; a prefix token sees only its
// own; finished Turns land in the history with normalized tokens.
func TestFleetEventsAndTurnHistory(t *testing.T) {
	h, biz := prefixHarness(t)
	owner := sse(t, h, "/v1/events", "test-token-0123456789", "")
	until(t, owner, "event: head")

	// The prefix token's stream: it opens with its own token, read from a fresh one.
	res := h.api("POST", "/v1/tokens", `{"name":"biz2","prefix":"biz-"}`)
	secret := decodeBody[api.Token](t, res).Secret
	lane := sse(t, h, "/v1/events", secret, "")
	until(t, lane, "event: head")

	if r := biz("PUT", "/v1/agent-computers/biz-a", bizSpec+"}"); r.StatusCode != 200 {
		t.Fatalf("create = %d", r.StatusCode)
	}
	until(t, owner, `"type":"fleet.changed"`)
	got := until(t, lane, `"type":"fleet.changed"`)
	if !strings.Contains(got[len(got)-1], `"names":["biz-a"]`) || !strings.Contains(got[len(got)-1], `"by":"token:bizapp"`) {
		t.Fatalf("fleet.changed = %s", got[len(got)-1])
	}

	scout := h.cds.get("scout")
	if r := h.api("POST", "/v1/agent-computers/scout/inbox", `{"body":"hi"}`); r.StatusCode != 201 {
		t.Fatalf("send = %d", r.StatusCode)
	}
	until(t, owner, `"type":"turn.dispatched"`)
	scout.set(func(f *fakeComputerd) {
		f.emitLocked(api.EventTextDelta, f.active, api.TextDeltaData{Text: "pong"})
		f.emitLocked(api.EventTurnFinished, f.active, map[string]any{"status": "completed", "usage": map[string]int{"inputTokens": 3}, "tokens": api.Tokens{Input: 3, Output: 1}})
		f.active = ""
	})
	seen := until(t, owner, `"type":"turn.finished"`)
	if !strings.Contains(strings.Join(seen, "\n"), `"agentComputer":"scout"`) {
		t.Fatalf("events lack agentComputer: %q", seen)
	}

	// Last-Event-ID resumes after an id without repeating it.
	var lastID string
	for _, l := range seen {
		if strings.HasPrefix(l, "id: ") {
			lastID = strings.TrimPrefix(l, "id: ")
		}
	}
	h.c.publishControllerEvent("scout", api.EventPhase, api.PhaseData{Phase: api.PhaseReady}, nil, nil)
	resumed := sse(t, h, "/v1/events?agentComputers=scout", "test-token-0123456789", lastID)
	first := until(t, resumed, "data: ")
	if !strings.Contains(first[len(first)-1], `"type":"agent_computer.phase"`) {
		t.Fatalf("resume replayed %q", first)
	}

	waitFor(t, func() bool {
		return len(decodeBody[api.TurnsResponse](t, h.api("GET", "/v1/agent-computers/scout/turns?limit=5", "")).Turns) == 1
	})
	turns := decodeBody[api.TurnsResponse](t, h.api("GET", "/v1/agent-computers/scout/turns", "")).Turns
	tr := turns[0]
	if tr.AgentComputer != "scout" || tr.Text != "pong" || tr.Status != "completed" || tr.Tokens == nil || tr.Tokens.Input != 3 || len(tr.Messages) != 1 {
		b, _ := json.Marshal(tr)
		t.Fatalf("turn = %s", b)
	}
	wantOutOfPrefix(t, "scout's turns", biz("GET", "/v1/agent-computers/scout/turns", ""))

	// Nothing of scout reached the prefix token's stream.
	for {
		select {
		case l := <-lane:
			if strings.Contains(l, `"scout"`) {
				t.Fatalf("the prefix token saw %s", l)
			}
			continue
		default:
		}
		break
	}
}

// A Turn is in Turn history by the time turn.finished shows on either event stream, even while storing it is slow.
func TestTurnStoredBeforeTurnFinished(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	fleetStream := sse(t, h, "/v1/events?agentComputers=scout", "test-token-0123456789", "")
	until(t, fleetStream, "event: head")
	if r := h.api("POST", "/v1/agent-computers/scout/inbox", `{"body":"hi"}`); r.StatusCode != 201 {
		t.Fatalf("send = %d", r.StatusCode)
	}
	until(t, fleetStream, `"type":"turn.dispatched"`)
	computerStream := sse(t, h, "/v1/agent-computers/scout/events", "test-token-0123456789", "")

	// Another writer holds fleet.db's write lock, so storing the Turn waits for it.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h.dir, "fleet.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	h.cds.get("scout").finish(api.TurnCompleted)
	quiet := time.After(300 * time.Millisecond)
	for waiting := true; waiting; {
		select {
		case l := <-fleetStream:
			if strings.Contains(l, `"type":"turn.finished"`) {
				t.Fatal("turn.finished reached GET /v1/events before the Turn was stored")
			}
		case l := <-computerStream:
			if strings.Contains(l, `"type":"turn.finished"`) {
				t.Fatal("turn.finished reached the agent computer's events before the Turn was stored")
			}
		case <-quiet:
			waiting = false
		}
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	for _, stream := range []<-chan string{fleetStream, computerStream} {
		until(t, stream, `"type":"turn.finished"`)
		if turns := decodeBody[api.TurnsResponse](t, h.api("GET", "/v1/agent-computers/scout/turns", "")).Turns; len(turns) != 1 || turns[0].Status != api.TurnCompleted {
			t.Fatalf("turns right after turn.finished = %+v", turns)
		}
	}
}
