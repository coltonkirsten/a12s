package controller

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

func TestFiles(t *testing.T) {
	if exec.Command("stat", "-c", "%s", "/").Run() != nil {
		t.Skip("the files scripts use GNU or busybox stat, as agent computers have; this stat is BSD")
	}
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\nhello\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "blob"), []byte{0, 1, 2, 0xff}, 0o644)
	os.Mkdir(filepath.Join(dir, "memory"), 0o755)
	os.WriteFile(filepath.Join(dir, ".hidden"), nil, 0o644)
	q := func(p string) string { return "/v1/agent-computers/scout/files?path=" + url.QueryEscape(p) }

	// A folder: folders first, hidden names included.
	d := decodeBody[api.FileResponse](t, h.api("GET", q(dir), ""))
	var names []string
	for _, e := range d.Entries {
		names = append(names, e.Name+":"+e.Kind)
	}
	if d.Kind != "dir" || strings.Join(names, " ") != "memory:dir .hidden:file blob:file notes.md:file" || d.Entries[3].Size != 14 || d.Entries[3].ModTime == "" {
		t.Fatalf("dir = %+v", d)
	}
	// Text comes as UTF-8, anything else as base64.
	if f := decodeBody[api.FileResponse](t, h.api("GET", q(filepath.Join(dir, "notes.md")), "")); f.Encoding != "utf-8" || f.Content != "# Notes\nhello\n" || f.Size != 14 || f.Truncated {
		t.Fatalf("text = %+v", f)
	}
	if f := decodeBody[api.FileResponse](t, h.api("GET", q(filepath.Join(dir, "blob")), "")); f.Encoding != "base64" || f.Content != base64.StdEncoding.EncodeToString([]byte{0, 1, 2, 0xff}) {
		t.Fatalf("binary = %+v", f)
	}
	// A write makes the folder, replaces the file and is journaled.
	target := filepath.Join(dir, "memory", "new", "a.md")
	if res := h.api("PUT", q(target), "corrected"); res.StatusCode != http.StatusNoContent {
		t.Fatalf("put = %d %+v", res.StatusCode, apiError(t, res))
	}
	if b, _ := os.ReadFile(target); string(b) != "corrected" {
		t.Fatalf("written = %q", b)
	}
	if diags := h.cds.get("scout").diagnostics; len(diags) == 0 || !strings.Contains(diags[len(diags)-1], "file written") {
		t.Fatalf("diagnostics = %v", diags)
	}
	for _, c := range []struct {
		path string
		code api.ErrorCode
	}{{filepath.Join(dir, "nope"), api.ErrNotFound}, {"relative/x", api.ErrInvalid}, {dir + "/../x", api.ErrInvalid}} {
		if e := apiError(t, h.api("GET", q(c.path), "")); e.Code != c.code {
			t.Fatalf("%s = %+v, want %s", c.path, e, c.code)
		}
	}
	// Asleep: nothing to read until it wakes.
	h.api("POST", "/v1/agent-computers/scout/power", `{"state":"asleep"}`)
	h.waitPhase("scout", api.PhaseAsleep)
	if e := apiError(t, h.api("GET", q(dir), "")); e.Code != api.ErrAsleep {
		t.Fatalf("asleep = %+v", e)
	}
}

func TestParseFileReadTruncated(t *testing.T) {
	// The cut fell inside "é": the partial character is dropped, the file is still text.
	f, err := parseFileRead("/x", "file\n10/0\nab\xc3")
	if err != nil || f.Encoding != "utf-8" || f.Content != "ab" || !f.Truncated {
		t.Fatalf("truncated = %+v %v", f, err)
	}
}

func TestTerminal(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	srv := httptest.NewServer(h.c.Handler())
	t.Cleanup(srv.Close)

	// Without the Upgrade it is refused.
	if res := h.api("GET", "/v1/agent-computers/scout/terminal", ""); res.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("no upgrade = %d", res.StatusCode)
	}

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /v1/agent-computers/scout/terminal?rows=30&cols=100 HTTP/1.1\r\nHost: x\r\n"+
		"Authorization: Bearer test-token-0123456789\r\nConnection: Upgrade\r\nUpgrade: a12s-terminal\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %v %v", res, err)
	}
	id := res.Header.Get("A12s-Terminal")
	io.WriteString(conn, "ls\n")
	got := make([]byte, 3)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "LS\n" {
		t.Fatalf("echo = %q %v", got, err)
	}
	if n, _ := h.d.Sessions(t.Context(), "scout"); n != 1 {
		t.Fatalf("sessions while open = %d", n)
	}

	// Resize finds the TTY by id, on its own computer only.
	if res := h.api("POST", "/v1/agent-computers/scout/terminal/"+id+"/resize", `{"rows":40,"cols":120}`); res.StatusCode != http.StatusNoContent {
		t.Fatalf("resize = %d", res.StatusCode)
	}
	if e := apiError(t, h.api("POST", "/v1/agent-computers/builder/terminal/"+id+"/resize", `{"rows":40,"cols":120}`)); e.Code != api.ErrNotFound {
		t.Fatalf("resize elsewhere = %+v", e)
	}
	tty := h.d.terminals[0]
	tty.mu.Lock()
	sizes := strings.Join(tty.sizes, " ")
	tty.mu.Unlock()
	if sizes != "30x100 40x120" {
		t.Fatalf("sizes = %s", sizes)
	}

	// Hanging up ends the shell: no session is left and both ends are journaled.
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, _ := h.d.Sessions(t.Context(), "scout")
		cd := h.cds.get("scout")
		cd.mu.Lock()
		diags := strings.Join(cd.diagnostics, "|")
		cd.mu.Unlock()
		if n == 0 && strings.Contains(diags, "terminal opened") && strings.Contains(diags, "terminal closed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after hang-up: sessions %d, diagnostics %s", n, diags)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var e api.Error
	json.NewDecoder(h.api("POST", "/v1/agent-computers/scout/terminal/"+id+"/resize", `{"rows":1,"cols":1}`).Body).Decode(&e)
	if e.Code != api.ErrNotFound {
		t.Fatalf("resize after close = %+v", e)
	}
}
