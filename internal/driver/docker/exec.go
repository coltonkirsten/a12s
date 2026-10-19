package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
)

func (d *Driver) Exec(ctx context.Context, computer string, cmd []string, stdin io.Reader, asRoot bool) (int, string, error) {
	if len(cmd) == 0 {
		return -1, "", fmt.Errorf("exec %s: empty command", computer)
	}
	if _, err := d.runningComputer(ctx, computer); err != nil {
		return -1, "", fmt.Errorf("exec %s: %w", computer, err)
	}
	user := engine.AgentUser
	if asRoot {
		user = "0:0"
	}
	return d.eng.Exec(ctx, d.ContainerName(computer), computer, user, cmd, stdin)
}

// Connect works only from a process on the Computer's internal network (the controller).
func (d *Driver) Connect(ctx context.Context, computer, service string) (net.Conn, error) {
	if service != driver.ServiceComputerd {
		return nil, fmt.Errorf("connect %s: unknown service %q", computer, service)
	}
	st, err := d.runningComputer(ctx, computer)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", computer, err)
	}
	addr, err := netip.ParseAddr(st.Address)
	if err != nil {
		return nil, fmt.Errorf("connect %s: no address on %s", computer, st.Network)
	}
	nres, err := d.cli.NetworkInspect(ctx, st.Network, client.NetworkInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", computer, err)
	}
	var subnets []netip.Prefix
	for _, c := range nres.Network.IPAM.Config {
		subnets = append(subnets, c.Subnet)
	}
	if on, err := onLocalNetwork(subnets); err != nil {
		return nil, fmt.Errorf("connect %s: %w", computer, err)
	} else if !on {
		return nil, fmt.Errorf("connect %s: %w %s; Connect must run in the controller container (%s)",
			computer, driver.ErrNotOnNetwork, st.Network, d.ControllerName())
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), strconv.Itoa(driver.ComputerdPort)))
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", computer, err)
	}
	return conn, nil
}

func (d *Driver) runningComputer(ctx context.Context, computer string) (driver.ComputerState, error) {
	res, err := d.cli.ContainerInspect(ctx, d.ContainerName(computer), client.ContainerInspectOptions{})
	if engine.IsNotFound(err) {
		return driver.ComputerState{}, driver.ErrNotFound
	}
	if err != nil {
		return driver.ComputerState{}, err
	}
	if res.Container.Config.Labels[engine.LabelFleet] != d.fleet || res.Container.Config.Labels[engine.LabelRole] != engine.RoleComputer {
		return driver.ComputerState{}, fmt.Errorf("container %s is not an agent computer of fleet %s", d.ContainerName(computer), d.fleet)
	}
	st := engine.ComputerState(computer, d.NetworkName(computer), res.Container)
	if !st.Running {
		return st, fmt.Errorf("computer is %s, not running", st.Power)
	}
	return st, nil
}

func onLocalNetwork(subnets []netip.Prefix) (bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false, fmt.Errorf("list interfaces: %w", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipnet.IP)
		if !ok {
			continue
		}
		for _, s := range subnets {
			if s.Contains(ip.Unmap()) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (d *Driver) Sessions(ctx context.Context, computer string) (int, error) {
	return d.eng.Sessions(ctx, d.ContainerName(computer))
}
