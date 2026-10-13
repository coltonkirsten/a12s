package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

func TestParseInterspersed(t *testing.T) {
	g := &globals{fleetPath: "fleet.yaml"}
	fs := g.flags("send")
	fresh := fs.Bool("fresh", false, "")
	pos, err := parse(fs, []string{"scout", "--fresh", "-f", "x/fleet.yaml", "hello there", "--", "--not-a-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if !*fresh || g.fleetPath != "x/fleet.yaml" || fmt.Sprint(pos) != "[scout hello there --not-a-flag]" {
		t.Errorf("fresh=%v fleet=%s pos=%q", *fresh, g.fleetPath, pos)
	}
	if _, err := parse(g.flags("x"), []string{"--bogus"}); err == nil {
		t.Error("unknown flag accepted")
	} else if _, ok := err.(usageError); !ok {
		t.Errorf("err %T, want usageError", err)
	}
}

func TestExitCodes(t *testing.T) {
	var b bytes.Buffer
	if code := run([]string{"nope"}, &b); code != 2 || !strings.Contains(b.String(), `unknown command "nope"`) {
		t.Errorf("code %d: %s", code, b.String())
	}
	b.Reset()
	if code := run([]string{"stop"}, &b); code != 2 || !strings.HasPrefix(b.String(), "a12s: stop needs one agent computer name") {
		t.Errorf("code %d: %s", code, b.String())
	}
	b.Reset()
	if code := run([]string{"-f", filepath.Join(t.TempDir(), "missing.yaml"), "status"}, &b); code != 1 || !strings.HasPrefix(b.String(), "a12s: read fleet") {
		t.Errorf("code %d: %s", code, b.String())
	}
}

// up sends a preset directory as files and a seccomp path as the profile; instructions and a preset CLAUDE.md
// together are refused.
func TestUpInlinesPresetsAndSeccomp(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "presets/scout/.claude/skills/x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{"presets/scout/CLAUDE.md": "hi\n", "presets/scout/.claude/skills/x/SKILL.md": "skill\n", "p.json": `{"defaultAction":"SCMP_ACT_ALLOW"}`} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	yaml := "apiVersion: a12s.ai/v1alpha1\nkind: Fleet\nmetadata: { name: t }\n" +
		"defaults: { harness: { kind: claude-code, version: 2.1.280 }, image: slim }\n" +
		"agent_computers:\n  scout: { home: { preset: ./presets/scout }, security: { seccomp: ./p.json } }\n  plain: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &globals{fleetPath: filepath.Join(dir, "fleet.yaml")}
	spec, err := fleet.LoadSpec(g.fleetPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.inline(spec); err != nil {
		t.Fatal(err)
	}
	f, err := spec.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	scout := f.Computers["scout"]
	if scout.Home.Preset != "" || scout.Instructions != "hi\n" || len(scout.Files) != 1 || scout.Files[".claude/skills/x/SKILL.md"] != "skill\n" || string(scout.Security.Seccomp.Profile) != `{"defaultAction":"SCMP_ACT_ALLOW"}` {
		t.Fatalf("scout = %+v", scout)
	}

	both := strings.Replace(yaml, "home: { preset: ./presets/scout }", "home: { preset: ./presets/scout }, instructions: be brief", 1)
	if err := os.WriteFile(filepath.Join(dir, "fleet.yaml"), []byte(both), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, _ = fleet.LoadSpec(g.fleetPath)
	if err := g.inline(spec); err == nil || !strings.Contains(err.Error(), "also has CLAUDE.md") {
		t.Fatalf("instructions and a preset CLAUDE.md = %v", err)
	}
}

func TestTurnPrinter(t *testing.T) {
	var out, errOut bytes.Buffer
	p := &turnPrinter{turn: "t_41", kind: "codex", out: &out, errOut: &errOut, tty: true}
	ev := func(typ api.EventType, data string) api.Event {
		return api.Event{Turn: "t_41", Type: typ, Data: json.RawMessage(data)}
	}
	for _, e := range []api.Event{
		ev(api.EventTurnStarted, `{"harness":{"kind":"codex","version":"0.154.0"},"model":"gpt-6-astra","resumed":false,"prompt":"p","cwd":"/home/agent"}`),
		ev(api.EventTextDelta, `{"text":"Adding a "}`),
		ev(api.EventTextDelta, `{"text":"health check."}`),
		ev(api.EventToolStarted, `{"name":"apply_patch"}`),
		ev(api.EventTextDelta, `{"text":"Done.\n"}`),
		{Turn: "t_other", Type: api.EventTextDelta, Data: json.RawMessage(`{"text":"not ours"}`)},
	} {
		if stop, _ := p.handle(e); stop {
			t.Fatal("stopped early")
		}
	}
	if stop, _ := p.handle(ev(api.EventTurnFinished, `{"status":"completed"}`)); !stop || p.status != "completed" {
		t.Fatal("turn.finished did not stop")
	}
	// Routing and filtering, not wording: this turn's text streams whole to
	// stdout, tools go to stderr, another turn's text is dropped.
	so, se := out.String(), errOut.String()
	if !strings.Contains(so, "Adding a health check.\nDone.\n") || !strings.Contains(so, "t_41") || strings.Contains(so, "not ours") || strings.Contains(so, "apply_patch") {
		t.Errorf("stdout %q", so)
	}
	if !strings.Contains(se, "apply_patch") || strings.Contains(se, "health check") || strings.Contains(se, "not ours") {
		t.Errorf("stderr %q", se)
	}
}

// TestSendWaitEndToEnd runs `a12s send --wait` against a fake Controller that drops
// the event stream mid-turn: the output has every delta exactly once.
func TestSendWaitEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var enq api.EnqueueRequest
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/status":
			_ = json.NewEncoder(w).Encode(api.StatusResponse{Fleet: "lab", AgentComputers: []api.ComputerStatus{{Name: "scout", Phase: api.PhaseBusy, ActiveTurn: "t_6"}}})
		case r.Method == "POST" && r.URL.Path == "/v1/agent-computers/scout/inbox":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&enq)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.EnqueueResponse{ID: "m_1", Seq: 1, State: api.MsgPending, Position: 0, Phase: api.PhaseBusy})
		case r.Method == "GET" && r.URL.Path == "/v1/agent-computers/scout/inbox/m_1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			snap, _ := json.Marshal(api.Event{V: 1, AgentComputer: "scout", Seq: 1, Type: "snapshot", Data: json.RawMessage(`{"message":{"id":"m_1","state":"pending"},"held":false}`)})
			fmt.Fprintf(w, "id: 1\ndata: %s\n\n", snap)
			del, _ := json.Marshal(api.Event{V: 1, AgentComputer: "scout", Seq: 3, Type: "delivered", Data: json.RawMessage(`{"turn":"t_7","batch":"b_1"}`)})
			fmt.Fprintf(w, "id: 3\ndata: %s\n\n", del)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == "GET" && r.URL.Path == "/v1/agent-computers/scout/events" && r.URL.Query().Get("turn") == "t_7":
			mu.Lock()
			conns++
			n := conns
			mu.Unlock()
			send := func(seq int64, typ api.EventType, data string) {
				b, _ := json.Marshal(api.Event{V: 1, AgentComputer: "scout", Turn: "t_7", Seq: seq, Type: typ, Data: json.RawMessage(data)})
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", seq, b)
				w.(http.Flusher).Flush()
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if n == 1 {
				send(1, api.EventTurnStarted, `{"harness":{"kind":"claude-code","version":"2.1.280"},"model":"claude-fable-5-1","resumed":true,"prompt":"p","cwd":"/home/agent","inbox":{"batch":"b_1","messages":[{"id":"m_1","sender":"local"},{"id":"m_0","sender":"cron:nightly-audit"}]}}`)
				send(2, api.EventTextDelta, `{"text":"The word "}`)
				panic(http.ErrAbortHandler)
			}
			if r.Header.Get("Last-Event-ID") != "2" {
				http.Error(w, "bad cursor", http.StatusBadRequest)
				return
			}
			send(2, api.EventTextDelta, `{"text":"The word "}`)
			send(3, api.EventTextDelta, `{"text":"was raven."}`)
			send(4, api.EventTurnFinished, `{"status":"completed"}`)
			<-r.Context().Done()
		default:
			http.Error(w, r.Method+" "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	var port int
	fmt.Sscanf(srv.URL[strings.LastIndex(srv.URL, ":")+1:], "%d", &port)
	if err := cli.WriteControllerFile("lab", &api.ControllerFile{Port: port, Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(home, ".a12s/lab/controller.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("controller.json mode %v", fi.Mode())
	}

	stdout, stderr := capture(t, func() {
		code := run([]string{"-f", "../../internal/fleet/testdata/lab.yaml", "send", "scout", "--wait", "--fresh", "what", "was", "the", "word?"}, os.Stderr)
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	})
	want := "turn t_7 started (claude-code 2.1.280, claude-fable-5-1, resumed: true, batch of 2: yours + cron:nightly-audit)\nThe word was raven.\nturn t_7 completed\n"
	if stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "[stream] reconnecting after seq 2") || !strings.Contains(stderr, "queued: scout is busy with turn t_6 (0 messages ahead)") {
		t.Errorf("stderr %q", stderr)
	}
	if enq.Body != "what was the word?" || !enq.Fresh || len(enq.IdempotencyKey) != 36 {
		t.Errorf("enqueue %+v", enq)
	}
}

func capture(t *testing.T, fn func()) (string, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	read := func() (*os.File, chan string) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		ch := make(chan string)
		go func() {
			b, _ := io.ReadAll(r)
			ch <- string(b)
		}()
		return w, ch
	}
	wOut, chOut := read()
	wErr, chErr := read()
	os.Stdout, os.Stderr = wOut, wErr
	fn()
	wOut.Close()
	wErr.Close()
	return <-chOut, <-chErr
}

// A partial failure still prints the summary, lists each failure, and fails the command.
func TestFinishUpPartialFailure(t *testing.T) {
	dir := t.TempDir()
	g := &globals{fleetPath: filepath.Join(dir, "fleet.yaml")}
	resp := &api.PutFleetResponse{
		Created: []string{"scout", "tester"},
		Errors:  map[string]string{"tester": "image a12s/browser:dev not found"},
	}
	var out, errOut bytes.Buffer
	err := g.finishUp(resp, false, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "1 agent computer(s) or Service(s) did not converge") {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "created scout, tester\n" {
		t.Errorf("stdout %q", out.String())
	}
	if !strings.Contains(errOut.String(), "tester: image a12s/browser:dev not found\n") {
		t.Errorf("stderr %q", errOut.String())
	}
	if code := report(io.Discard, command{name: "up"}, g.finishUp(resp, false, io.Discard, io.Discard)); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

func TestStaleGatewayTokensSparesLiveSiblings(t *testing.T) {
	tokens := []api.Token{{Name: "gateway-7012"}, {Name: "gateway-7013"}, {Name: "gateway-7014"}, {Name: "gateway"}, {Name: "gateway-ci"}, {Name: "app"}}
	live := map[int]bool{7013: true}
	got := staleGatewayTokens(tokens, 7012, func(p int) bool { return live[p] })
	if strings.Join(got, ",") != "gateway-7012,gateway-7014" {
		t.Fatalf("stale = %v", got)
	}
}

func TestPageMaySaysTheScopeInOneLine(t *testing.T) {
	got := pageMay(api.Token{Name: "gateway-7012", Actions: []string{"status", "send"}, Computers: []string{"lead", "worker1"}})
	want := "the page may read status, send Messages, on agent computers lead, worker1 (token:gateway-7012; change with --allow, --agent-computers)"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
