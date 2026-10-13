package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
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

// internalTarget is a separate, mandatory check that never consults egress policy: a Service is
// reachable only on a port both active generations agree on, at its observed address on its own network.
func (c *Controller) internalTarget(ctx context.Context, caller, host string, port int, plain bool) (netip.AddrPort, int, string) {
	name := strings.TrimSuffix(host, ".a12s")
	rc, ok := c.reachOf(caller)
	if !ok {
		return netip.AddrPort{}, http.StatusForbidden, "unknown caller"
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
	kind := "agent_computers"
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
