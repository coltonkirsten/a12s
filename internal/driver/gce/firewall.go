package gce

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

// Ports a Computer VM serves the Controller on, and the Controller ports it may reach.
const (
	EnginePort = 2376
	ProxyPort  = 3128
	MTLSPort   = 7443
)

const firewallFix = "create the fleet's firewall rules (docs/drivers/gce.md#network-and-firewall)"

// checkFirewall verifies the VPC rules the fleet's bootstrap created; the driver never edits firewalls. A Computer VM
// may send only to the Controller's proxy and mTLS ports, and only the Controller may reach its engine and computerd.
func (d *Driver) checkFirewall(ctx context.Context) error {
	rules, err := d.api.ListFirewalls(ctx, "")
	if err != nil {
		return fmt.Errorf("check firewall rules: %w", err)
	}
	network := ""
	if in, err := d.api.GetSubnetwork(ctx, d.cfg.Subnetwork); err == nil {
		network = last(in.Network)
	}
	var applies []gceapi.Firewall
	for _, r := range rules {
		if r.Disabled || (network != "" && last(r.Network) != network) {
			continue
		}
		if len(r.TargetServiceAccounts) > 0 || (len(r.TargetTags) > 0 && !slices.Contains(r.TargetTags, NetworkTag)) {
			continue
		}
		applies = append(applies, r)
	}
	ctl := d.cfg.Controller
	var deny *gceapi.Firewall
	for i, r := range applies {
		if r.Direction == "EGRESS" && slices.Contains(r.TargetTags, NetworkTag) && slices.Contains(r.DestinationRanges, "0.0.0.0/0") && deniesAll(r) {
			if deny == nil || r.Priority < deny.Priority {
				deny = &applies[i]
			}
		}
	}
	if deny == nil {
		return fmt.Errorf("no firewall rule denies all egress from instances tagged %s; fix: %s", NetworkTag, firewallFix)
	}
	reach := map[int]bool{}
	for _, r := range applies {
		if r.Direction != "EGRESS" || len(r.Allowed) == 0 || r.Priority >= deny.Priority {
			continue
		}
		if !onlyHost(r.DestinationRanges, ctl) {
			return fmt.Errorf("firewall rule %s lets instances tagged %s send to %s ahead of %s; fix: limit it to %s/32", r.Name, NetworkTag, strings.Join(r.DestinationRanges, ","), deny.Name, ctl)
		}
		for _, p := range []int{ProxyPort, MTLSPort} {
			if allows(r.Allowed, p) {
				reach[p] = true
			}
		}
		if extra := extraPorts(r.Allowed, ProxyPort, MTLSPort); extra != "" {
			return fmt.Errorf("firewall rule %s lets instances tagged %s reach the Controller on %s; fix: allow only tcp:%d,%d", r.Name, NetworkTag, extra, ProxyPort, MTLSPort)
		}
	}
	if !reach[ProxyPort] || !reach[MTLSPort] {
		return fmt.Errorf("no firewall rule lets instances tagged %s reach the Controller (%s) on tcp:%d,%d; fix: %s", NetworkTag, ctl, ProxyPort, MTLSPort, firewallFix)
	}
	inbound := map[int]bool{}
	for _, r := range applies {
		if r.Direction != "INGRESS" && r.Direction != "" || len(r.Allowed) == 0 || !slices.Contains(r.TargetTags, NetworkTag) {
			continue
		}
		fromController := slices.Contains(r.SourceServiceAccounts, d.cfg.ControllerAccount) || onlyHost(r.SourceRanges, ctl) && len(r.SourceTags) == 0
		for _, p := range []int{EnginePort, MTLSPort} {
			if allows(r.Allowed, p) {
				if !fromController {
					return fmt.Errorf("firewall rule %s opens tcp:%d on instances tagged %s to more than the Controller; fix: restrict its source to the Controller's service account", r.Name, p, NetworkTag)
				}
				inbound[p] = true
			}
		}
	}
	if !inbound[EnginePort] || !inbound[MTLSPort] {
		return fmt.Errorf("no firewall rule lets the Controller reach instances tagged %s on tcp:%d,%d; fix: %s", NetworkTag, EnginePort, MTLSPort, firewallFix)
	}
	return nil
}

func deniesAll(r gceapi.Firewall) bool {
	for _, d := range r.Denied {
		if d.IPProtocol == "all" {
			return true
		}
	}
	return false
}

func onlyHost(ranges []string, host netip.Addr) bool {
	if len(ranges) == 0 {
		return false
	}
	for _, s := range ranges {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return false
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Bits() != p.Addr().BitLen() || p.Addr() != host {
			return false
		}
	}
	return true
}

func allows(rules []gceapi.FirewallRule, port int) bool {
	for _, r := range rules {
		if r.IPProtocol != "tcp" && r.IPProtocol != "all" {
			continue
		}
		if len(r.Ports) == 0 {
			return true
		}
		for _, p := range r.Ports {
			lo, hi := portRange(p)
			if lo <= port && port <= hi {
				return true
			}
		}
	}
	return false
}

// extraPorts names what the rules allow beyond the given tcp ports.
func extraPorts(rules []gceapi.FirewallRule, ok ...int) string {
	for _, r := range rules {
		if r.IPProtocol != "tcp" {
			return r.IPProtocol
		}
		if len(r.Ports) == 0 {
			return "every tcp port"
		}
		for _, p := range r.Ports {
			lo, hi := portRange(p)
			for port := lo; port <= hi; port++ {
				if !slices.Contains(ok, port) {
					return "tcp:" + p
				}
			}
		}
	}
	return ""
}

func portRange(s string) (int, int) {
	a, b, found := strings.Cut(s, "-")
	lo, _ := strconv.Atoi(a)
	if !found {
		return lo, lo
	}
	hi, _ := strconv.Atoi(b)
	return lo, hi
}

func last(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}
