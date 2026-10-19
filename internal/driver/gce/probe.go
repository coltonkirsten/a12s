package gce

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/moby/moby/client"
)

// Probe opens one Computer VM's engine outside the Controller, from the Controller VM, for the GCE exit gate: its
// in-VM firewall and sizing checks run commands in the container and read the engine's stats.
func Probe(ctx context.Context, fleet string, s Settings, clientCert tls.Certificate, computer string) (Guest, *client.Client, error) {
	host, err := controllerVM(ctx, s.Zone)
	if err != nil {
		return nil, nil, err
	}
	in, err := host.api.GetInstance(ctx, InstanceName(fleet, computer))
	if err != nil {
		return nil, nil, fmt.Errorf("VM of %s: %w", computer, err)
	}
	e := &Engines{API: host.api, Fleet: fleet, Controller: host.address, ClientCert: clientCert, Wait: time.Minute}
	g, err := e.Open(ctx, vmOf(in))
	if err != nil {
		return nil, nil, err
	}
	return g, g.(*vmGuest).eng.Client(), nil
}
