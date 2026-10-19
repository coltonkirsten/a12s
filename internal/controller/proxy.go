package controller

import (
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

const ProxyPort = 3128

// Policy is the Fleet's egress.allow plus the bundle's signInHosts.
type Policy struct {
	Hosts map[string]bool
	// AllowAll is `allow: ["*"]`: any public host on any port.
	AllowAll bool
}

func NewPolicy(allow, signIn []string) Policy {
	p := Policy{Hosts: map[string]bool{}}
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

// Check returns why a CONNECT tunnel to host:port is denied, or "" when it
// is allowed. Addresses are checked separately, after resolution.
func (p Policy) Check(host string, port int) string {
	if p.AllowAll {
		return ""
	}
	if !p.Hosts[normalizeHost(host)] {
		return "host is not in the egress allowlist"
	}
	if port != 443 {
		return "only port 443 is allowed"
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

func BlockedAddr(addr netip.Addr) string {
	a := addr.Unmap()
	if !a.IsValid() {
		return "invalid address"
	}
	// A zoned address belongs to no prefix (Prefix.Contains is false), so it
	// would slip past every check below.
	if a.Zone() != "" {
		return "address " + a.String() + " has an IPv6 zone"
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

// Proxy is the egress proxy. Identify names the Computer behind a connection from the connection's local and remote
// addresses (the Driver's Caller).
type Proxy struct {
	Identify func(local, remote netip.Addr) (computer string, ok bool)
	Policy   func(computer string) (Policy, bool)
	Resolve  func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error)
	Denied   func(computer, host string, port int, reason string)
	// Internal decides a <name>.a12s target before and independently of Policy. plain is an
	// absolute-URI (non-TLS) request. Nil: .a12s names are denied.
	Internal func(ctx context.Context, caller, host string, port int, plain bool) (netip.AddrPort, int, string)
}

// IsInternal reports whether host is in the .a12s name space, which only
// Internal decides and which never reaches Policy, DNS or a redirect.
func IsInternal(host string) bool {
	h := normalizeHost(host)
	return h == "a12s" || strings.HasSuffix(h, ".a12s")
}

func NewProxy(identify func(local, remote netip.Addr) (string, bool), policy func(string) (Policy, bool), denied func(string, string, int, string)) *Proxy {
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

// ServeHTTP refuses plaintext so every allowed destination is reached over TLS; `*` keeps
// any port, still over CONNECT.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	computer, ok := p.identify(r)
	if !ok {
		http.Error(w, "a12s egress: request from an unknown network", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodConnect {
		if host, port, ok := plainTarget(r); ok {
			if IsInternal(host) {
				p.serveInternalHTTP(w, r, computer, host, port)
				return
			}
			p.deny(w, computer, host, port, "plain http is not proxied; use https")
			return
		}
		http.Error(w, "a12s egress: only CONNECT is proxied", http.StatusBadRequest)
		return
	}
	h, ps, err := net.SplitHostPort(r.Host)
	port, perr := strconv.Atoi(ps)
	if err != nil || perr != nil || port < 1 || port > 65535 {
		http.Error(w, "a12s egress: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	host := h
	if strings.Contains(host, "%") {
		p.deny(w, computer, host, port, "IPv6 zones are not allowed")
		return
	}
	if IsInternal(host) {
		target, ok := p.internal(w, r, computer, host, port, false)
		if !ok {
			return
		}
		upstream, err := p.Dial(r.Context(), "tcp", target.String())
		if err != nil {
			http.Error(w, "a12s egress: "+err.Error(), http.StatusBadGateway)
			return
		}
		p.tunnel(w, upstream)
		return
	}
	policy, _ := p.Policy(computer)
	reason := policy.Check(host, port)
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
	p.tunnel(w, upstream)
}

// internal decides a .a12s target; on refusal it has answered and journaled.
func (p *Proxy) internal(w http.ResponseWriter, r *http.Request, caller, host string, port int, plain bool) (netip.AddrPort, bool) {
	host = normalizeHost(host)
	if p.Internal == nil {
		p.deny(w, caller, host, port, "no internal names on this proxy")
		return netip.AddrPort{}, false
	}
	target, status, reason := p.Internal(r.Context(), caller, host, port, plain)
	if reason == "" {
		return target, true
	}
	if p.Denied != nil {
		go p.Denied(caller, host, port, reason)
	}
	if status == 0 {
		status = http.StatusForbidden
	}
	http.Error(w, fmt.Sprintf("a12s egress: %s: %s", net.JoinHostPort(host, strconv.Itoa(port)), reason), status)
	return netip.AddrPort{}, false
}

// serveInternalHTTP forwards a plain-HTTP absolute-URI request to a .a12s
// Service (the one plaintext the proxy carries; on the Docker driver both
// legs stay on the host's bridges).
func (p *Proxy) serveInternalHTTP(w http.ResponseWriter, r *http.Request, caller, host string, port int) {
	target, ok := p.internal(w, r, caller, host, port, true)
	if !ok {
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = "http"
	out.Host = r.URL.Host
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	tr := &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return p.Dial(ctx, "tcp", target.String()) },
		DisableKeepAlives: true,
		Proxy:             nil,
	}
	defer tr.CloseIdleConnections()
	res, err := tr.RoundTrip(out)
	if err != nil {
		http.Error(w, "a12s egress: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer res.Body.Close()
	for k, vs := range res.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
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
			_ = rc.Flush() // streams (MCP over SSE) pass through as they come
		}
		if err != nil {
			return
		}
	}
}

func plainTarget(r *http.Request) (string, int, bool) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		return "", 0, false
	}
	port := 80
	if r.URL.Scheme == "https" {
		port = 443
	}
	if ps := r.URL.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil {
			return "", 0, false
		}
		port = n
	}
	return r.URL.Hostname(), port, true
}

func (p *Proxy) identify(r *http.Request) (string, bool) {
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	local, ok := la.(*net.TCPAddr)
	if !ok || p.Identify == nil {
		return "", false
	}
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return "", false
	}
	return p.Identify(local.AddrPort().Addr().Unmap(), remote.Addr().Unmap())
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
