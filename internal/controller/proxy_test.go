package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPolicyCheck(t *testing.T) {
	p := NewPolicy([]string{"api.openai.com", "GitHub.com."}, []string{"platform.claude.com"}, false)
	for _, tc := range []struct {
		host   string
		port   int
		tunnel bool
		ok     bool
	}{
		{"api.openai.com", 443, true, true},
		{"github.com", 443, true, true},
		{"platform.claude.com", 443, true, true},
		{"API.OPENAI.COM.", 443, true, true},
		{"api.openai.com", 22, true, false},
		{"evil.com", 443, true, false},
		{"sub.api.openai.com", 443, true, false},
		{"api.openai.com", 80, false, true},
		{"api.openai.com", 8080, false, false},
		{"evil.com", 80, false, false},
	} {
		if got := p.Check(tc.host, tc.port, tc.tunnel) == ""; got != tc.ok {
			t.Errorf("Check(%s, %d, tunnel=%v) allowed=%v, want %v", tc.host, tc.port, tc.tunnel, got, tc.ok)
		}
	}
	all := NewPolicy([]string{"*"}, nil, false)
	if all.Check("anything.example", 22, true) != "" || all.Check("x.example", 8080, false) != "" {
		t.Error(`"*" must allow any host and port`)
	}
	open := NewPolicy(nil, nil, true)
	if open.Check("anything.example", 5432, true) != "" {
		t.Error("unenforced must allow any host")
	}
}

func TestBlockedAddr(t *testing.T) {
	blocked := []string{
		"10.1.2.3", "172.16.0.1", "192.168.1.1", "127.0.0.1", "169.254.169.254", "100.100.100.200",
		"0.0.0.0", "224.0.0.1", "255.255.255.255", "10.213.0.2",
		"::1", "::", "fe80::1", "fd00:ec2::254", "fc00::1", "ff02::1", "::ffff:10.0.0.1",
		"::ffff:169.254.169.254", "64:ff9b::a00:1", "2002:a00:1::1", "2001:db8::1",
	}
	for _, s := range blocked {
		if BlockedAddr(netip.MustParseAddr(s)) == "" {
			t.Errorf("%s is allowed", s)
		}
	}
	for _, s := range []string{"93.184.216.34", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if why := BlockedAddr(netip.MustParseAddr(s)); why != "" {
			t.Errorf("%s blocked: %s", s, why)
		}
	}
}

type proxyHarness struct {
	srv    *httptest.Server
	mu     sync.Mutex
	dialed []string
	denied []string
}

// newProxyHarness serves a Proxy for Computer "scout". DNS is a fixed map;
// every dial of a validated address lands on target instead.
func newProxyHarness(t *testing.T, policy Policy, dns map[string][]string, target string) *proxyHarness {
	h := &proxyHarness{}
	p := &Proxy{
		Identify: func(netip.Addr) (string, bool) { return "scout", true },
		Policy:   func(string) (Policy, bool) { return policy, true },
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, s := range dns[host] {
				out = append(out, netip.MustParseAddr(s))
			}
			if out == nil {
				return nil, fmt.Errorf("no such host")
			}
			return out, nil
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			h.mu.Lock()
			h.dialed = append(h.dialed, addr)
			h.mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, target)
		},
		Denied: func(computer, host string, port int, reason string) {
			h.mu.Lock()
			h.denied = append(h.denied, fmt.Sprintf("%s %s:%d", computer, host, port))
			h.mu.Unlock()
		},
	}
	h.srv = httptest.NewServer(p)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *proxyHarness) connect(t *testing.T, hostport string) (int, net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", h.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostport, hostport)
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, c
}

func TestProxyConnectDialsTheValidatedAddress(t *testing.T) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err == nil {
			io.Copy(c, c)
			c.Close()
		}
	}()
	h := newProxyHarness(t, NewPolicy([]string{"api.openai.com"}, nil, false),
		map[string][]string{"api.openai.com": {"93.184.216.34"}}, echo.Addr().String())
	code, c := h.connect(t, "api.openai.com:443")
	defer c.Close()
	if code != 200 {
		t.Fatalf("CONNECT = %d", code)
	}
	fmt.Fprint(c, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("tunnel echo = %q, %v", buf, err)
	}
	if len(h.dialed) != 1 || h.dialed[0] != "93.184.216.34:443" {
		t.Fatalf("dialed %v", h.dialed)
	}
}

func TestProxyDenies(t *testing.T) {
	h := newProxyHarness(t, NewPolicy([]string{"api.openai.com", "rebind.example"}, nil, false),
		map[string][]string{"api.openai.com": {"93.184.216.34"}, "rebind.example": {"93.184.216.34", "169.254.169.254"}}, "127.0.0.1:1")
	for _, target := range []string{"evil.example:443", "api.openai.com:22", "rebind.example:443"} {
		code, c := h.connect(t, target)
		c.Close()
		if code != http.StatusForbidden {
			t.Errorf("CONNECT %s = %d, want 403", target, code)
		}
	}
	if len(h.dialed) != 0 {
		t.Errorf("dialed %v", h.dialed)
	}
	// "*" still never reaches private or metadata addresses, by name or literal.
	all := newProxyHarness(t, NewPolicy([]string{"*"}, nil, false), map[string][]string{"internal.example": {"10.0.0.5"}}, "127.0.0.1:1")
	for _, target := range []string{"internal.example:443", "169.254.169.254:80", "[::1]:443"} {
		code, c := all.connect(t, target)
		c.Close()
		if code != http.StatusForbidden {
			t.Errorf(`"*" CONNECT %s = %d, want 403`, target, code)
		}
	}
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.denied) == 3 })
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProxyPlainHTTP(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s host=%s proxyauth=%q", r.Method, r.URL.Path, r.Host, r.Header.Get("Proxy-Authorization"))
	}))
	defer origin.Close()
	h := newProxyHarness(t, NewPolicy([]string{"deb.example"}, nil, false),
		map[string][]string{"deb.example": {"93.184.216.34"}}, origin.Listener.Addr().String())
	proxyURL, _ := url.Parse(h.srv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	req, _ := http.NewRequest("GET", "http://deb.example/dists/x", nil)
	req.Header.Set("Proxy-Authorization", "secret")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != `GET /dists/x host=deb.example proxyauth=""` {
		t.Fatalf("got %d %q", res.StatusCode, body)
	}
	res, err = client.Get("http://other.example/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || strings.Count(strings.TrimSpace(string(body)), "\n") != 0 {
		t.Fatalf("denied = %d %q", res.StatusCode, body)
	}
}
