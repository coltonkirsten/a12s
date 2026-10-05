package controller

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/fleet"
)

// ProxyPort is the egress proxy's port on every Computer network.
const ProxyPort = 3128

// Policy is one Computer's egress policy (§6): the Fleet's egress.allow plus
// its bundle's signInHosts.
type Policy struct {
	Hosts map[string]bool
	// AllowAll is `allow: ["*"]`: any public host on any port.
	AllowAll bool
	// Unenforced is the Fleet's `egress.enforcement: unenforced`: the
	// proxy still runs but allows every public destination.
	Unenforced bool
}

// NewPolicy builds a policy from allowlist entries and sign-in hosts.
func NewPolicy(allow, signIn []string, unenforced bool) Policy {
	p := Policy{Hosts: map[string]bool{}, Unenforced: unenforced}
	for _, h := range append(append([]string(nil), allow...), signIn...) {
		if h == fleet.AllowAll {
			p.AllowAll = true
			continue
		}
		p.Hosts[normalizeHost(h)] = true
	}
	return p
}

func normalizeHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// Check returns why a request to host:port is denied, or "" when it is
// allowed. tunnel is true for CONNECT, false for a plain http:// request.
// Addresses are checked separately, after resolution.
func (p Policy) Check(host string, port int, tunnel bool) string {
	if p.AllowAll || p.Unenforced {
		return ""
	}
	if !p.Hosts[normalizeHost(host)] {
		return "host is not in the egress allowlist"
	}
	if tunnel && port != 443 {
		return "only port 443 is allowed"
	}
	if !tunnel && port != 80 {
		return "plain http is only allowed on port 80"
	}
	return ""
}

// blockedPrefixes are never dialed, whatever the policy: private,
// loopback, link-local and metadata (169.254.0.0/16, fd00:ec2::254 in
// fc00::/7), carrier-grade NAT, multicast, documentation, and transition
// prefixes that can embed a private IPv4 address.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32",
		"2002::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// BlockedAddr returns why addr may not be dialed, or "" for a public unicast
// address.
func BlockedAddr(addr netip.Addr) string {
	a := addr.Unmap()
	if !a.IsValid() {
		return "invalid address"
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return "address " + a.String() + " is not public (" + p.String() + ")"
		}
	}
	if !a.IsGlobalUnicast() {
		return "address " + a.String() + " is not public"
	}
	return ""
}

// Proxy is the egress proxy. The Computer behind a connection is the one
// whose network the connection arrived on: Identify maps the proxy's local
// address to it.
type Proxy struct {
	Identify func(local netip.Addr) (computer string, ok bool)
	Policy   func(computer string) (Policy, bool)
	// Resolve looks up a host; Dial connects to a validated address.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	// Denied is told about every denied request (it journals a diagnostic).
	Denied func(computer, host string, port int, reason string)
}

// NewProxy returns a proxy that resolves with the system resolver.
func NewProxy(identify func(netip.Addr) (string, bool), policy func(string) (Policy, bool), denied func(string, string, int, string)) *Proxy {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &Proxy{
		Identify: identify,
		Policy:   policy,
		Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		Dial:   d.DialContext,
		Denied: denied,
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	computer, ok := p.identify(r)
	if !ok {
		http.Error(w, "a12s egress: request from an unknown network", http.StatusForbidden)
		return
	}
	tunnel := r.Method == http.MethodConnect
	var host string
	var port int
	switch {
	case tunnel:
		h, ps, err := net.SplitHostPort(r.Host)
		n, perr := strconv.Atoi(ps)
		if err != nil || perr != nil || n < 1 || n > 65535 {
			http.Error(w, "a12s egress: CONNECT needs host:port", http.StatusBadRequest)
			return
		}
		host, port = h, n
	case r.URL.IsAbs() && r.URL.Scheme == "http" && r.URL.Host != "":
		host, port = r.URL.Hostname(), 80
		if ps := r.URL.Port(); ps != "" {
			n, err := strconv.Atoi(ps)
			if err != nil || n < 1 || n > 65535 {
				http.Error(w, "a12s egress: bad port", http.StatusBadRequest)
				return
			}
			port = n
		}
	default:
		http.Error(w, "a12s egress: only CONNECT and absolute http:// requests are proxied", http.StatusBadRequest)
		return
	}

	policy, _ := p.Policy(computer)
	reason := policy.Check(host, port, tunnel)
	var addrs []netip.Addr
	if reason == "" {
		addrs, reason = p.resolve(r.Context(), host)
	}
	if reason != "" {
		p.deny(w, computer, host, port, reason)
		return
	}
	upstream, err := p.dialAny(r.Context(), addrs, port)
	if err != nil {
		http.Error(w, "a12s egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	if tunnel {
		p.tunnel(w, upstream)
		return
	}
	p.forward(w, r, upstream)
}

func (p *Proxy) identify(r *http.Request) (string, bool) {
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	tcp, ok := la.(*net.TCPAddr)
	if !ok || p.Identify == nil {
		return "", false
	}
	return p.Identify(tcp.AddrPort().Addr().Unmap())
}

func (p *Proxy) deny(w http.ResponseWriter, computer, host string, port int, reason string) {
	if p.Denied != nil {
		go p.Denied(computer, host, port, reason)
	}
	http.Error(w, fmt.Sprintf("a12s egress: %s denied: %s", net.JoinHostPort(host, strconv.Itoa(port)), reason), http.StatusForbidden)
}

// resolve returns the addresses to dial, all of them public, or why not.
// Any non-public answer rejects the host (DNS rebinding).
func (p *Proxy) resolve(ctx context.Context, host string) ([]netip.Addr, string) {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if why := BlockedAddr(a); why != "" {
			return nil, why
		}
		return []netip.Addr{a.Unmap()}, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := p.Resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, "host does not resolve"
	}
	for i, a := range addrs {
		if why := BlockedAddr(a); why != "" {
			return nil, "host resolves to a non-public address: " + why
		}
		addrs[i] = a.Unmap()
	}
	return addrs, ""
}

func (p *Proxy) dialAny(ctx context.Context, addrs []netip.Addr, port int) (net.Conn, error) {
	var errs []error
	for _, a := range addrs {
		conn, err := p.Dial(ctx, "tcp", netip.AddrPortFrom(a, uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, fmt.Errorf("connect failed: %w", errors.Join(errs...))
}

func (p *Proxy) tunnel(w http.ResponseWriter, upstream net.Conn) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		upstream.Close()
		http.Error(w, "a12s egress: cannot tunnel", http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	defer upstream.Close()
	_ = conn.SetDeadline(time.Time{})
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		// Bytes the client sent after CONNECT may already be buffered.
		_, _ = io.Copy(upstream, brw.Reader)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		closeWrite(conn)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// hopHeaders are removed when forwarding plain http requests and responses.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func stripHop(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, k := range strings.Split(f, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// forward sends one plain http request over upstream (one request per
// connection) and copies the response back.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, upstream net.Conn) {
	defer upstream.Close()
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header = r.Header.Clone()
	stripHop(out.Header)
	out.Close = true
	if err := out.Write(upstream); err != nil {
		http.Error(w, "a12s egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	res, err := http.ReadResponse(bufio.NewReader(upstream), out)
	if err != nil {
		http.Error(w, "a12s egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	stripHop(res.Header)
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}
