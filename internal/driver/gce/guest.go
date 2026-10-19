package gce

import (
	"context"
	"io"
	"net/netip"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// Guest is the in-VM half of a Computer: the VM's own engine and the one Computer container in it. The driver's
// other half (VMs, disks, service accounts) never reaches inside a VM except through a Guest.
type Guest interface {
	// Materialize makes the container match spec: its images loaded from the Controller, its bundle extracted, the
	// container created with the engine package's settings and its files copied. It is a no-op when a container of
	// that generation already finished materializing.
	Materialize(ctx context.Context, spec driver.ComputerSpec) error
	// Container reports the Computer container's state; exists is false before the first Materialize.
	Container(ctx context.Context) (st driver.ComputerState, exists bool, err error)
	Start(ctx context.Context) error
	// Stop gives the harness the engine's StopTimeout to flush.
	Stop(ctx context.Context) error
	Exec(ctx context.Context, user string, cmd []string, stdin io.Reader) (exitCode int, output string, err error)
	Sessions(ctx context.Context) (int, error)
	// Events streams the container's deaths until ctx ends or the engine goes away.
	Events(ctx context.Context) <-chan driver.Event
	Close() error
}

// VM is what opening a Guest needs to know about its instance.
type VM struct {
	Name     string
	ID       uint64
	Computer string
	Address  netip.Addr
	// Started tells boots apart: a Guest opened before a restart is stale after it.
	Started string
}

// GuestOpener opens a VM's engine, waiting for it to answer while the VM boots.
type GuestOpener func(ctx context.Context, vm VM) (Guest, error)
