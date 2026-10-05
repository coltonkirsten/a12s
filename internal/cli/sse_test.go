package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

func writeEvent(t *testing.T, w http.ResponseWriter, seq int64, typ api.EventType, data string) {
	t.Helper()
	ev := api.Event{V: 1, Computer: "scout", Turn: "t_1", Seq: seq, Type: typ, Data: json.RawMessage(data)}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", seq, b)
	w.(http.Flusher).Flush()
}

func testClient(url string) *Client {
	return &Client{BaseURL: url, Token: "tok", HTTP: &http.Client{}}
}

// TestStreamReconnectExactlyOnce drops the connection mid-stream, in the
// middle of an event, and checks every event is delivered once and in order.
func TestStreamReconnectExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	var conns []string // Last-Event-ID per connection ("" for none)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/computers/scout/events" || r.URL.Query().Get("turn") != "t_1" {
			http.Error(w, "bad path "+r.URL.String(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		cursor, has := r.Header.Get("Last-Event-ID"), r.Header["Last-Event-Id"] != nil
		if !has {
			cursor = "<none>"
		}
		conns = append(conns, cursor)
		n := len(conns)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			writeEvent(t, w, 1, api.EventTurnStarted, `{"harness":{"kind":"claude-code","version":"2.1.280"},"resumed":false,"prompt":"p","cwd":"/home/agent"}`)
			writeEvent(t, w, 2, api.EventTextDelta, `{"text":"hel"}`)
			writeEvent(t, w, 3, api.EventTextDelta, `{"text":"lo"}`)
			// Half an event, then the connection dies.
			fmt.Fprint(w, "id: 4\ndata: {\"v\":1,\"seq\":4,\"type\":\"text.del")
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		case 2:
			// The server replays from the cursor; it may overlap.
			writeEvent(t, w, 3, api.EventTextDelta, `{"text":"lo"}`)
			writeEvent(t, w, 4, api.EventTextDelta, `{"text":" world"}`)
			panic(http.ErrAbortHandler) // drop again, cleanly between events
		default:
			writeEvent(t, w, 5, api.EventToolStarted, `{"name":"Bash"}`)
			writeEvent(t, w, 6, api.EventTurnFinished, `{"status":"completed"}`)
			<-r.Context().Done() // a live stream stays open
		}
	}))
	defer srv.Close()

	var got []int64
	var text strings.Builder
	reconnects := 0
	s := &Stream{
		Client:      testClient(srv.URL),
		Path:        "/v1/computers/scout/events?turn=t_1",
		OnReconnect: func(int64, error) { reconnects++ },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := s.Run(ctx, func(e api.Event) (bool, error) {
		got = append(got, e.Seq)
		if e.Type == api.EventTextDelta {
			var d api.TextDeltaData
			_ = json.Unmarshal(e.Data, &d)
			text.WriteString(d.Text)
		}
		return e.Type == api.EventTurnFinished, nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []int64{1, 2, 3, 4, 5, 6}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("delivered %v, want %v", got, want)
	}
	if text.String() != "hello world" {
		t.Errorf("text %q", text.String())
	}
	if want := []string{"<none>", "3", "4"}; fmt.Sprint(conns) != fmt.Sprint(want) {
		t.Errorf("Last-Event-ID per connection %q, want %q", conns, want)
	}
	if reconnects != 2 {
		t.Errorf("reconnects = %d, want 2", reconnects)
	}
}

func TestStreamCursorAndGap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Last-Event-ID") != "41" {
			http.Error(w, "want cursor 41", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive\n\nevent: gap\ndata: {\"oldest\":100}\n\n")
	}))
	defer srv.Close()
	s := &Stream{Client: testClient(srv.URL), Path: "/v1/computers/scout/events", After: 41, SendCursor: true}
	err := s.Run(context.Background(), func(api.Event) (bool, error) {
		t.Error("no event expected")
		return false, nil
	})
	var gap *GapError
	if !errors.As(err, &gap) || gap.After != 41 {
		t.Fatalf("err = %v, want a gap after 41", err)
	}
}

func TestStreamIdleEnds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent(t, w, 7, api.EventLifecycle, `{"phase":"Ready"}`)
		writeEvent(t, w, 8, api.EventLifecycle, `{"phase":"Busy"}`)
		<-r.Context().Done()
	}))
	defer srv.Close()
	var got []int64
	s := &Stream{Client: testClient(srv.URL), Path: "/x", SendCursor: true, Idle: 200 * time.Millisecond}
	err := s.Run(context.Background(), func(e api.Event) (bool, error) {
		got = append(got, e.Seq)
		return false, nil
	})
	if err != nil || fmt.Sprint(got) != "[7 8]" {
		t.Fatalf("got %v, err %v", got, err)
	}
}

func TestStreamFatalStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"code":"asleep","message":"scout is asleep"}`)
	}))
	defer srv.Close()
	s := &Stream{Client: testClient(srv.URL), Path: "/x"}
	err := s.Run(context.Background(), func(api.Event) (bool, error) { return false, nil })
	if !HasCode(err, api.ErrAsleep) {
		t.Fatalf("err = %v, want asleep", err)
	}
}
