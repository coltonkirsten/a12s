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
	fs := g.flags("prompt")
	fresh := fs.Bool("new", false, "")
	pos, err := parse(fs, []string{"scout", "--new", "-f", "x/fleet.yaml", "hello there", "--", "--not-a-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if !*fresh || g.fleetPath != "x/fleet.yaml" || fmt.Sprint(pos) != "[scout hello there --not-a-flag]" {
		t.Errorf("new=%v fleet=%s pos=%q", *fresh, g.fleetPath, pos)
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
	if code := run([]string{"stop"}, &b); code != 2 || !strings.HasPrefix(b.String(), "a12s: stop needs one Computer name") {
		t.Errorf("code %d: %s", code, b.String())
	}
	b.Reset()
	if code := run([]string{"-f", filepath.Join(t.TempDir(), "missing.yaml"), "status"}, &b); code != 1 || !strings.HasPrefix(b.String(), "a12s: read fleet") {
		t.Errorf("code %d: %s", code, b.String())
	}
}

func TestPresetsPinAndRefuseChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "presets/scout"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "presets/scout/CLAUDE.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &globals{fleetPath: filepath.Join(dir, "fleet.yaml")}
	f := &fleet.Fleet{Computers: map[string]fleet.Computer{
		"scout": {Home: fleet.Home{Preset: "./presets/scout"}},
		"twin":  {Home: fleet.Home{Preset: "./presets/scout"}},
		"plain": {},
	}}
	lock := fleet.NewLock()
	tars, err := g.presets(f, lock)
	if err != nil {
		t.Fatal(err)
	}
	if len(tars) != 1 || len(tars["./presets/scout"]) == 0 || !strings.HasPrefix(lock.Presets["./presets/scout"], "sha256:") {
		t.Fatalf("tars %d, lock %v", len(tars), lock.Presets)
	}
	if got := addedPins(fleet.NewLock(), lock); fmt.Sprint(got) != "[./presets/scout]" {
		t.Errorf("addedPins = %v", got)
	}
	if got := addedPins(lock, cloneLock(lock)); len(got) != 0 {
		t.Errorf("addedPins of a clone = %v", got)
	}
	// Same content: fine. Changed content: the pin is not silently moved.
	if _, err := g.presets(f, lock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "presets/scout/CLAUDE.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.presets(f, lock); err == nil || !strings.Contains(err.Error(), "changed since fleet.lock pinned it") {
		t.Errorf("err = %v", err)
	}
}

func TestPutFleetRequestJSON(t *testing.T) {
	req := api.PutFleetRequest{Asleep: true, Presets: map[string][]byte{"./p": []byte("tar")}}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["asleep"] != true || m["presets"].(map[string]any)["./p"] != "dGFy" {
		t.Errorf("json %s", b)
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
	want := "turn t_41 started (codex 0.154.0, gpt-6-astra, resumed: false)\nAdding a health check.\nDone.\nturn t_41 completed\n"
	if out.String() != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", out.String(), want)
	}
	if errOut.String() != "[tool] apply_patch started\n" {
		t.Errorf("stderr %q", errOut.String())
	}
}

// TestPromptEndToEnd runs `a12s prompt` against a fake Controller that drops
// the event stream mid-turn: the output has every delta exactly once.
func TestPromptEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var turnReq api.TurnRequest
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/computers/scout/turns":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&turnReq)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(api.TurnResponse{TurnID: "t_7", SessionID: "s_1"})
		case r.Method == "GET" && r.URL.Path == "/v1/computers/scout/events" && r.URL.Query().Get("turn") == "t_7":
			mu.Lock()
			conns++
			n := conns
			mu.Unlock()
			send := func(seq int64, typ api.EventType, data string) {
				b, _ := json.Marshal(api.Event{V: 1, Computer: "scout", Turn: "t_7", Seq: seq, Type: typ, Data: json.RawMessage(data)})
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", seq, b)
				w.(http.Flusher).Flush()
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if n == 1 {
				send(1, api.EventTurnStarted, `{"harness":{"kind":"claude-code","version":"2.1.280"},"model":"claude-fable-5-1","resumed":true,"prompt":"p","cwd":"/home/agent"}`)
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
		code := run([]string{"-f", "../../internal/fleet/testdata/lab.yaml", "prompt", "scout", "what", "was", "the", "word?"}, os.Stderr)
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	})
	want := "turn t_7 started (claude-code 2.1.280, claude-fable-5-1, resumed: true)\nThe word was raven.\nturn t_7 completed\n"
	if stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "[stream] reconnecting after seq 2") {
		t.Errorf("stderr %q", stderr)
	}
	if turnReq.Prompt != "what was the word?" || turnReq.Session != api.SessionCurrent || len(turnReq.RequestID) != 36 {
		t.Errorf("turn request %+v", turnReq)
	}
}

// capture runs fn with os.Stdout and os.Stderr redirected.
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

// A partial failure still writes new pins and the summary, lists each
// failure, and fails the command.
func TestFinishUpPartialFailure(t *testing.T) {
	dir := t.TempDir()
	g := &globals{fleetPath: filepath.Join(dir, "fleet.yaml")}
	onDisk := fleet.NewLock()
	resp := &api.PutFleetResponse{
		Created: []string{"scout", "tester"},
		Errors:  map[string]string{"tester": "image a12s/browser:dev not found"},
		Lock:    &fleet.Lock{Images: map[string]string{"slim": "sha256:aa"}},
	}
	var out, errOut bytes.Buffer
	err := g.finishUp(onDisk, fleet.NewLock(), resp, false, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "1 Computer(s) did not converge") {
		t.Fatalf("err = %v", err)
	}
	if out.String() != "created scout, tester (asleep: none)\n" {
		t.Errorf("stdout %q", out.String())
	}
	if !strings.Contains(errOut.String(), "tester: image a12s/browser:dev not found\n") {
		t.Errorf("stderr %q", errOut.String())
	}
	l, err := fleet.LoadLock(g.lockPath())
	if err != nil || l.Images["slim"] != "sha256:aa" {
		t.Errorf("lock %+v, %v", l, err)
	}
	if code := report(io.Discard, command{name: "up"}, g.finishUp(onDisk, fleet.NewLock(), resp, false, io.Discard, io.Discard)); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}
