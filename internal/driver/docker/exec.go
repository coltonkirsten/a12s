package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// Exec runs cmd in a running Computer as root or as the agent (1000:1000),
// feeding stdin if given, and returns the exit code and combined output.
func (d *Driver) Exec(ctx context.Context, computer string, cmd []string, stdin io.Reader, asRoot bool) (int, string, error) {
	if len(cmd) == 0 {
		return -1, "", fmt.Errorf("exec %s: empty command", computer)
	}
	if _, err := d.runningComputer(ctx, computer); err != nil {
		return -1, "", fmt.Errorf("exec %s: %w", computer, err)
	}
	user := agentUser
	if asRoot {
		user = "0:0"
	}
	created, err := d.cli.ExecCreate(ctx, d.ContainerName(computer), client.ExecCreateOptions{
		User:         user,
		Cmd:          cmd,
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return -1, "", fmt.Errorf("exec %s: %w", computer, err)
	}
	att, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return -1, "", fmt.Errorf("exec %s: attach: %w", computer, err)
	}
	defer att.Close()

	stdinErr := make(chan error, 1)
	if stdin != nil {
		go func() {
			_, err := io.Copy(att.Conn, stdin)
			if cerr := att.CloseWrite(); err == nil {
				err = cerr
			}
			stdinErr <- err
		}()
	} else {
		stdinErr <- nil
	}
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, att.Reader); err != nil {
		return -1, out.String(), fmt.Errorf("exec %s: read output: %w", computer, err)
	}
	select {
	case err := <-stdinErr:
		if err != nil {
			return -1, out.String(), fmt.Errorf("exec %s: write stdin: %w", computer, err)
		}
	case <-ctx.Done():
		return -1, out.String(), fmt.Errorf("exec %s: %w", computer, ctx.Err())
	}
	// The output stream ends as the process exits; the exit code can lag a moment.
	for {
		res, err := d.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return -1, out.String(), fmt.Errorf("exec %s: inspect: %w", computer, err)
		}
		if !res.Running {
			return res.ExitCode, out.String(), nil
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return -1, out.String(), fmt.Errorf("exec %s: %w", computer, ctx.Err())
		}
	}
}

// Connect dials a service inside the Computer over the Computer's internal
// network. Only a process attached to that network, the controller
// container, can reach it; elsewhere Connect returns ErrNotOnNetwork.
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

// runningComputer observes one Computer and requires it to be running.
func (d *Driver) runningComputer(ctx context.Context, computer string) (driver.ComputerState, error) {
	res, err := d.cli.ContainerInspect(ctx, d.ContainerName(computer), client.ContainerInspectOptions{})
	if isNotFound(err) {
		return driver.ComputerState{}, driver.ErrNotFound
	}
	if err != nil {
		return driver.ComputerState{}, err
	}
	if res.Container.Config.Labels[LabelFleet] != d.fleet || res.Container.Config.Labels[LabelRole] != RoleComputer {
		return driver.ComputerState{}, fmt.Errorf("container %s is not a Computer of fleet %s", d.ContainerName(computer), d.fleet)
	}
	st := d.computerState(computer, res.Container)
	if !st.Running {
		return st, fmt.Errorf("computer is %s, not running", st.Power)
	}
	return st, nil
}

// onLocalNetwork reports whether this process has an interface address in
// any of subnets.
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
