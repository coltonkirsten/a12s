package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDevURL(t *testing.T) {
	for line, want := range map[string]string{
		"  \x1b[32m➜\x1b[39m  \x1b[1mLocal\x1b[22m:   \x1b[36mhttp://localhost:\x1b[1m5173\x1b[22m/\x1b[39m": "http://localhost:5173",
		"   - Local:        http://localhost:3000":                                                           "http://localhost:3000",
		"Listening on http://127.0.0.1:8080/app":                                                             "http://localhost:8080",
		"Server running at https://0.0.0.0:4443":                                                             "https://localhost:4443",
		"VITE v7.1.0  ready in 312 ms":                                                                       "",
		"see https://vite.dev:443/guide":                                                                     "",
	} {
		u, ok := DevURL(line)
		got := ""
		if ok {
			got = u.String()
		}
		if got != want {
			t.Errorf("DevURL(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestStartDevWaitsForTheURLAndStopsTheWholeGroup(t *testing.T) {
	script := `sleep 0.2; echo "  Local: http://localhost:5999/"; sleep 60 & wait`
	d, err := StartDev(context.Background(), []string{"sh", "-c", script}, 0, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if d.URL.String() != "http://localhost:5999" {
		t.Fatalf("URL = %s", d.URL)
	}
	start := time.Now()
	d.Stop()
	if time.Since(start) > 4*time.Second {
		t.Fatalf("Stop took %s: SIGTERM did not reach the process group", time.Since(start))
	}
}

func TestStartDevFailsWhenTheCommandExits(t *testing.T) {
	_, err := StartDev(context.Background(), []string{"sh", "-c", "echo port in use; exit 3"}, 0, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exited before it was ready") {
		t.Fatalf("err = %v", err)
	}
}

func TestDevProxyForwardsAllButTheAPIIncludingWebsockets(t *testing.T) {
	dev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			conn, rw, _ := w.(http.Hijacker).Hijack()
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			line, _ := rw.ReadString('\n')
			_, _ = rw.WriteString("echo " + line)
			_ = rw.Flush()
			return
		}
		_, _ = io.WriteString(w, "dev:"+r.URL.Path+" host="+r.Host)
	}))
	defer dev.Close()
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "controller") }))
	defer ctl.Close()
	cu, _ := url.Parse(ctl.URL)
	du, _ := url.Parse(dev.URL)
	gw := httptest.NewServer(Handler(Config{Controller: cu, Token: "scoped", Dev: du}))
	defer gw.Close()

	get := func(path string) string {
		res, err := http.Get(gw.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	host := strings.TrimPrefix(gw.URL, "http://")
	if got := get("/src/App.tsx"); got != "dev:/src/App.tsx host="+host {
		t.Errorf("app path: %q", got)
	}
	if got := get("/v1/status"); got != "controller" {
		t.Errorf("/v1/: %q", got)
	}
	if got := get("/a12s.js"); !strings.Contains(got, "A12sClient = class") {
		t.Errorf("/a12s.js is not the SDK")
	}

	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(conn, "GET /?token=x HTTP/1.1\r\nHost: "+host+"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	_, _ = io.WriteString(conn, "hmr\n")
	if line, _ := br.ReadString('\n'); line != "echo hmr\n" {
		t.Fatalf("websocket echo = %q", line)
	}
}
