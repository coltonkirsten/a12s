package gateway

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
)

func newGateway(t *testing.T, web fstest.MapFS) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		if r.URL.Path == "/v1/computers/lead/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: head\ndata: {\"seq\":3}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `{"fleet":"f"}`)
	}))
	t.Cleanup(ctl.Close)
	u, _ := url.Parse(ctl.URL)
	var h http.Handler
	if web == nil {
		h = Handler(u, "scoped", nil)
	} else {
		h = Handler(u, "scoped", web)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw, &seen
}

func TestProxyAddsTheScopedTokenAndDropsBrowserCredentials(t *testing.T) {
	gw, seen := newGateway(t, nil)
	req, _ := http.NewRequest("GET", gw.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer from-the-page")
	req.Header.Set("Cookie", "session=x")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(body) != `{"fleet":"f"}` {
		t.Fatalf("status = %d %s", res.StatusCode, body)
	}
	got := (*seen)[0]
	if got.Header.Get("Authorization") != "Bearer scoped" || got.Header.Get("Cookie") != "" {
		t.Fatalf("controller saw Authorization %q Cookie %q", got.Header.Get("Authorization"), got.Header.Get("Cookie"))
	}
}

func TestEventStreamsAreFlushed(t *testing.T) {
	gw, _ := newGateway(t, nil)
	res, err := http.Get(gw.URL + "/v1/computers/lead/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != "event: head\n" {
		t.Fatalf("first line = %q, %v", line, err)
	}
}

func TestRefusesNonLoopbackHostsAndCrossOriginWrites(t *testing.T) {
	gw, seen := newGateway(t, nil)
	do := func(method, host, origin string) int {
		req, _ := http.NewRequest(method, gw.URL+"/v1/computers/lead/inbox", strings.NewReader(`{"body":"x"}`))
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if s := do("POST", "evil.example:7012", ""); s != http.StatusForbidden {
		t.Errorf("rebinding host: %d", s)
	}
	if s := do("POST", "", "http://evil.example"); s != http.StatusForbidden {
		t.Errorf("cross-origin write: %d", s)
	}
	if len(*seen) != 0 {
		t.Fatalf("refused requests reached the Controller: %d", len(*seen))
	}
	if s := do("POST", "", "http://"+strings.TrimPrefix(gw.URL, "http://")); s != http.StatusOK {
		t.Errorf("same-origin write: %d", s)
	}
	if s := do("POST", "localhost:7012", ""); s != http.StatusOK {
		t.Errorf("localhost: %d", s)
	}
}

func TestServesTheSDKAndTheApp(t *testing.T) {
	get := func(gw *httptest.Server, path string) string {
		res, err := http.Get(gw.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	builtin, _ := newGateway(t, nil)
	if !strings.Contains(get(builtin, "/"), `import { connect } from "/a12s.js"`) {
		t.Error("the built-in dashboard is not served at /")
	}
	if !strings.Contains(get(builtin, "/a12s.js"), "A12sClient = class") {
		t.Error("/a12s.js is not the browser SDK")
	}
	app, _ := newGateway(t, fstest.MapFS{"index.html": {Data: []byte("my app")}})
	if get(app, "/") != "my app" || !strings.Contains(get(app, "/a12s.js"), "A12sClient = class") {
		t.Error("--web does not serve the app next to /a12s.js")
	}
}
