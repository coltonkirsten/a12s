package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// endpointsLocked names an MCP server a12s-<svc> when the Service has one MCP port, and
// a12s-<svc>-<port> when it has several. The result is part of the user's generation.
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

// reach is what the proxy needs to decide a .a12s connection.
type reach struct {
	uses    []driver.Endpoint // the caller's active uses
	service bool              // the caller is a Service
}

func (c *Controller) reachOf(name string) (reach, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.state.Services[name]; s != nil {
		return reach{uses: s.Active.Uses, service: true}, true
	}
	if r := c.state.Computers[name]; r != nil {
		return reach{uses: r.Active.Uses}, true
	}
	return reach{}, false
}

// servicePort reports whether port is a declared port of the Service's active generation, and the host it is
// dialed at when the Service is external.
func (c *Controller) servicePort(name string, port int) (external string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state.Services[name]
	if s == nil {
		return "", false
	}
	for _, p := range s.Active.Ports {
		if p.Port == port {
			return s.Active.External, true
		}
	}
	return "", false
}

// internalTarget is a separate, mandatory check that never consults egress policy: a Service is
// reachable only on a port both active generations agree on, at its observed address on its own network or, when
// external, at its host. It returns the host:port to dial.
func (c *Controller) internalTarget(ctx context.Context, caller, host string, port int, plain bool) (string, int, string) {
	name := strings.TrimSuffix(host, ".a12s")
	rc, ok := c.reachOf(caller)
	if !ok {
		return "", http.StatusForbidden, "unknown caller"
	}
	if strings.Contains(name, ".") || name == "" {
		return "", http.StatusForbidden, host + " is not a Service name"
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
	kind := "agent_computers"
	if rc.service {
		kind = "services"
	}
	switch {
	case !usesIt:
		return "", http.StatusForbidden, fmt.Sprintf("%s cannot reach %s: %s.%s.uses does not list %s", caller, name, kind, caller, name)
	case !usesPort:
		if plain {
			return "", http.StatusForbidden, fmt.Sprintf("%s:%d is not an http port %s uses", name, port, caller)
		}
		return "", http.StatusForbidden, fmt.Sprintf("%s:%d is not a declared port of %s", name, port, name)
	}
	external, ok := c.servicePort(name, port)
	switch {
	case !ok:
		return "", http.StatusForbidden, fmt.Sprintf("%s:%d is not a declared port of %s", name, port, name)
	case external != "" && !plain:
		return "", http.StatusForbidden, fmt.Sprintf("%s is external and a tunnel can't carry the %s header  fix: use plain http://%s.a12s:%d", name, CallerHeader, name, port)
	}
	if ph := c.servicePhase(name); ph != api.PhaseReady {
		return "", http.StatusServiceUnavailable, fmt.Sprintf("%s is %s", name, ph)
	}
	if external != "" {
		return net.JoinHostPort(external, strconv.Itoa(port)), 0, ""
	}
	addr, err := c.serviceAddr(ctx, name)
	if err != nil {
		return "", http.StatusServiceUnavailable, fmt.Sprintf("%s has no address: %v", name, err)
	}
	return netip.AddrPortFrom(addr, uint16(port)).String(), 0, ""
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
