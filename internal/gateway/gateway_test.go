package gateway

import (
	"bufio"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	cfg := Config{Controller: u, Token: "scoped", Key: "k"}
	if web != nil {
		cfg.Web = web
	}
	gw := httptest.NewServer(Handler(cfg))
	t.Cleanup(gw.Close)
	return gw, &seen
}

func TestProxyAddsTheScopedTokenAndDropsBrowserCredentials(t *testing.T) {
	gw, seen := newGateway(t, nil)
	req, _ := http.NewRequest("GET", gw.URL+"/v1/status", nil)
	req.Header.Set("Cookie", "session=x; "+keyCookie(gw))
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
	res, err := authed(gw).Get(gw.URL + "/v1/computers/lead/events")
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
		req.Header.Set("Authorization", "Bearer k")
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
	if s := do("GET", "192.168.1.20:7012", ""); s != http.StatusForbidden {
		t.Errorf("LAN address: %d", s)
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

func TestServesTheDashboardOrTheBuiltApp(t *testing.T) {
	get := func(gw *httptest.Server, path string) (int, string) {
		res, err := authed(gw).Get(gw.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	builtin, _ := newGateway(t, nil)
	if _, page := get(builtin, "/"); !strings.Contains(page, `<script type="module" src="/dashboard.js">`) {
		t.Error("the built-in dashboard is not served at /")
	}
	if _, js := get(builtin, "/dashboard.js"); !strings.Contains(js, "A12sClient = class") || strings.Contains(js, "\nexport ") {
		t.Error("/dashboard.js is not the dashboard bundled with the SDK, or it exports the SDK")
	}
	app, _ := newGateway(t, fstest.MapFS{
		"index.html":         {Data: []byte(`<script type="module" src="/assets/index-x1.js"></script>`)},
		"assets/index-x1.js": {Data: []byte("app code")},
	})
	if code, body := get(app, "/assets/index-x1.js"); code != http.StatusOK || body != "app code" {
		t.Errorf("--web does not serve a built app's absolute asset paths: %d %q", code, body)
	}
	for _, p := range []string{"/a12s.js", "/dashboard.js"} {
		if code, _ := get(app, p); code != http.StatusNotFound {
			t.Errorf("--web serves %s: %d", p, code)
		}
	}
}

func keyCookie(gw *httptest.Server) string {
	u, _ := url.Parse(gw.URL)
	return "a12s-gateway-" + u.Port() + "=k"
}

// authed is a browser that opened the link the gateway printed.
func authed(gw *httptest.Server) *http.Client {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	res, err := c.Get(gw.URL + "/?key=k")
	if err != nil {
		panic(err)
	}
	res.Body.Close()
	return c
}

func TestTheKeyBecomesACookieAndLeavesTheURL(t *testing.T) {
	gw, seen := newGateway(t, nil)
	jar, _ := cookiejar.New(nil)
	var redirects []string
	c := &http.Client{Jar: jar, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		redirects = append(redirects, r.URL.RequestURI())
		return nil
	}}
	res, err := c.Get(gw.URL + "/?key=k&tab=2")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(redirects) != 1 || redirects[0] != "/?tab=2" {
		t.Fatalf("redirects = %v", redirects)
	}
	u, _ := url.Parse(gw.URL)
	cookies := jar.Cookies(u)
	if len(cookies) != 1 || cookies[0].Name != "a12s-gateway-"+u.Port() {
		t.Fatalf("cookies = %v", cookies)
	}
	set := res.Request.Response.Header.Get("Set-Cookie")
	if !strings.Contains(set, "HttpOnly") || !strings.Contains(set, "SameSite=Strict") {
		t.Fatalf("Set-Cookie = %q", set)
	}
	res, err = c.Get(gw.URL + "/v1/status")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("with the cookie: %v %v", res.StatusCode, err)
	}
	res.Body.Close()
	if len(*seen) != 1 || (*seen)[0].Header.Get("Cookie") != "" {
		t.Fatal("the cookie reached the Controller")
	}
}

func TestWithoutTheKeyTheAPIIsLocked(t *testing.T) {
	gw, seen := newGateway(t, nil)
	status := func(path string, h ...string) int {
		req, _ := http.NewRequest("GET", gw.URL+path, nil)
		for i := 0; i < len(h); i += 2 {
			req.Header.Set(h[i], h[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	p := "/v1/status"
	if s := status(p); s != http.StatusUnauthorized {
		t.Errorf("%s without key: %d", p, s)
	}
	if s := status(p, "Authorization", "Bearer wrong"); s != http.StatusUnauthorized {
		t.Errorf("%s with a wrong key: %d", p, s)
	}
	if s := status(p, "Cookie", "a12s-gateway-1=k"); s != http.StatusUnauthorized {
		t.Errorf("%s with another port's cookie: %d", p, s)
	}
	if s := status(p, "Authorization", "Bearer k"); s != http.StatusOK {
		t.Errorf("%s with the key as a bearer token: %d", p, s)
	}
	if s := status("/?key=wrong"); s != http.StatusForbidden {
		t.Errorf("wrong ?key=: %d", s)
	}
	if s := status("/", "Sec-Fetch-Mode", "navigate"); s != http.StatusUnauthorized {
		t.Errorf("opening the page without the key: %d", s)
	}
	if len(*seen) != 1 {
		t.Fatalf("the Controller saw %d requests, want only the keyed one", len(*seen))
	}
}
