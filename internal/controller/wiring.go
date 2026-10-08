package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// endpointsLocked resolves a uses list against the applied Fleet's
// manifests: every declared port of every used Service, with its address
// variable and, for an MCP port the user wants as a tool, the MCP server
// name: a12s-<svc> when the Service has one MCP port, a12s-<svc>-<port>
// when it has several (§5.4). The result is part of the user's generation.
func (c *Controller) endpointsLocked(uses []fleet.Use) []driver.Endpoint {
	var out []driver.Endpoint
	for _, u := range uses {
		m := c.state.Manifests[u.Name]
		if m == nil {
			continue
		}
		mcpPorts := m.MCPPorts()
		for _, pn := range sortedPortNames(m) {
			p := m.Ports[pn]
			e := driver.Endpoint{Service: u.Name, Port: pn, Number: p.Port, Protocol: p.Proto(), Env: driver.EnvName("SERVICE", u.Name, pn)}
			if p.MCP != nil {
				e.MCPPath = p.MCP.Path
				if u.WantsMCP() {
					e.Tool = "a12s-" + u.Name
					if len(mcpPorts) > 1 {
						e.Tool += "-" + pn
					}
				}
			}
			out = append(out, e)
		}
	}
	return out
}

func sortedPortNames(m *fleet.ServiceManifest) []string {
	names := make([]string, 0, len(m.Ports))
	for n := range m.Ports {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// tcpEgressOf is a Service's named TCP egress as wired into it.
func tcpEgressOf(s fleet.Service) []driver.TCPEgress {
	var out []driver.TCPEgress
	for _, t := range s.Egress.TCP {
		out = append(out, driver.TCPEgress{Name: t.Name, Port: t.Port, Env: driver.EnvName("EGRESS", t.Name)})
	}
	return out
}

// reach is what the proxy needs to decide a .a12s connection.
type reach struct {
	uses    []driver.Endpoint // the caller's active uses
	tcp     []fleet.TCPEgress // the caller's named TCP egress (Services)
	service bool              // the caller is a Service
	private []string          // the Fleet's egress.allowPrivate when staged
}

// reachOf returns a caller's active reach.
func (c *Controller) reachOf(name string) (reach, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.state.Services[name]; s != nil {
		return reach{uses: s.Active.Uses, tcp: s.Active.TCP, service: true, private: s.Active.AllowPrivate}, true
	}
	if r := c.state.Computers[name]; r != nil {
		return reach{uses: r.Active.Uses}, true
	}
	return reach{}, false
}

// target is a Service's active declared port, for the proxy.
func (c *Controller) servicePort(name string, port int) (fleet.ServicePort, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state.Services[name]
	if s == nil {
		return fleet.ServicePort{}, false
	}
	for _, p := range s.Active.Ports {
		if p.Port == port {
			return p, true
		}
	}
	return fleet.ServicePort{}, false
}

// internalTarget decides a <name>.a12s connection for the proxy (§5.4): a
// separate, mandatory check that never consults egress policy. A Service
// is reachable only on a port the caller's active generation uses and the
// Service's active generation declares, at its observed address on its own
// network; <name>.egress.a12s is a Service's named TCP egress.
func (c *Controller) internalTarget(ctx context.Context, caller, host string, port int, plain bool) (netip.AddrPort, int, string) {
	name := strings.TrimSuffix(host, ".a12s")
	rc, ok := c.reachOf(caller)
	if !ok {
		return netip.AddrPort{}, http.StatusForbidden, "unknown caller"
	}
	if eg, ok := strings.CutSuffix(name, ".egress"); ok {
		return c.egressTarget(ctx, caller, rc, eg, port)
	}
	if strings.Contains(name, ".") || name == "" {
		return netip.AddrPort{}, http.StatusForbidden, host + " is not a Service name"
	}
	usesIt, usesPort := false, false
	for _, e := range rc.uses {
		if e.Service == name {
			usesIt = true
			if e.Number == port && (!plain || e.Protocol == fleet.ProtocolHTTP) {
				usesPort = true
			}
		}
	}
	kind := "computers"
	if rc.service {
		kind = "services"
	}
	switch {
	case !usesIt:
		return netip.AddrPort{}, http.StatusForbidden, fmt.Sprintf("%s cannot reach %s: %s.%s.uses does not list %s", caller, name, kind, caller, name)
	case !usesPort:
		if plain {
			return netip.AddrPort{}, http.StatusForbidden, fmt.Sprintf("%s:%d is not an http port %s uses", name, port, caller)
		}
		return netip.AddrPort{}, http.StatusForbidden, fmt.Sprintf("%s:%d is not a declared port of %s", name, port, name)
	}
	if _, ok := c.servicePort(name, port); !ok {
		return netip.AddrPort{}, http.StatusForbidden, fmt.Sprintf("%s:%d is not a declared port of %s", name, port, name)
	}
	if ph := c.servicePhase(name); ph != api.PhaseReady {
		return netip.AddrPort{}, http.StatusServiceUnavailable, fmt.Sprintf("%s is %s", name, ph)
	}
	addr, err := c.serviceAddr(ctx, name)
	if err != nil {
		return netip.AddrPort{}, http.StatusServiceUnavailable, fmt.Sprintf("%s has no address: %v", name, err)
	}
	return netip.AddrPortFrom(addr, uint16(port)), 0, ""
}

// serviceAddr is a running Service's address on its own network, read from
// the Driver (never resolved by name).
func (c *Controller) serviceAddr(ctx context.Context, name string) (netip.Addr, error) {
	st, ok, err := c.observeService(ctx, name)
	if err != nil {
		return netip.Addr{}, err
	}
	if !ok || !st.Running {
		return netip.Addr{}, errors.New("not running")
	}
	return netip.ParseAddr(st.Address)
}

// alwaysDenied are never reachable through named TCP egress, whatever
// allowPrivate says: loopback, link-local and metadata, unspecified,
// multicast.
var alwaysDenied = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "fe80::/10", "ff00::/8", "fd00:ec2::254/128"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// fleetPools are the Fleet's own networks: the internal-network pool and
// the Controller bridge. No Fleet field opens them.
func (c *Controller) fleetPools(ctx context.Context) []netip.Prefix {
	pools := []netip.Prefix{c.cfg.SubnetPool}
	if c.selfAddrs != nil {
		if addrs, err := c.selfAddrs(ctx); err == nil {
			if ip, ok := addrs["a12s-"+c.cfg.Fleet]; ok && ip.Is4() {
				p, _ := ip.Prefix(16)
				pools = append(pools, p)
			}
		}
	}
	return pools
}

// egressTarget resolves a Service's named TCP egress and validates the
// address it will dial: the Fleet's pools and loopback/link-local/metadata
// first (no Fleet field reads), then private ranges only within
// egress.allowPrivate, then pinned for this connection.
func (c *Controller) egressTarget(ctx context.Context, caller string, rc reach, name string, port int) (netip.AddrPort, int, string) {
	if !rc.service {
		return netip.AddrPort{}, http.StatusForbidden, "only a Service has named TCP egress"
	}
	var t *fleet.TCPEgress
	for i := range rc.tcp {
		if rc.tcp[i].Name == name {
			t = &rc.tcp[i]
		}
	}
	if t == nil || t.Port != port {
		return netip.AddrPort{}, http.StatusForbidden, fmt.Sprintf("%s has no egress.tcp entry %s on port %d", caller, name, port)
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(t.Host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		found, err := net.DefaultResolver.LookupNetIP(rctx, "ip", t.Host)
		if err != nil || len(found) == 0 {
			return netip.AddrPort{}, http.StatusBadGateway, t.Host + " does not resolve"
		}
		addrs = found
	}
	pools := c.fleetPools(ctx)
	for _, a := range addrs {
		a = a.Unmap()
		if why := egressAddrDenied(a, pools, rc.private); why != "" {
			return netip.AddrPort{}, http.StatusForbidden, t.Host + ": " + why
		}
	}
	return netip.AddrPortFrom(addrs[0].Unmap(), uint16(port)), 0, ""
}

// egressAddrDenied returns why a named-egress address may not be dialed.
func egressAddrDenied(a netip.Addr, pools []netip.Prefix, allowPrivate []string) string {
	if !a.IsValid() || a.Zone() != "" {
		return "invalid address"
	}
	for _, p := range alwaysDenied {
		if p.Contains(a) {
			return "address " + a.String() + " is never reachable (" + p.String() + ")"
		}
	}
	for _, p := range pools {
		if p.Contains(a) {
			return "address " + a.String() + " is inside the Fleet's own networks"
		}
	}
	if BlockedAddr(a) == "" {
		return ""
	}
	for _, s := range allowPrivate {
		if p, err := netip.ParsePrefix(s); err == nil && p.Contains(a) {
			return ""
		}
	}
	return "address " + a.String() + " is private  fix: add a range covering it to egress.allowPrivate"
}
