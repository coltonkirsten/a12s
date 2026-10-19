package gce

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

// Teardown is a12s down's half on GCE: it works without the Controller, from the Controller VM.
type Teardown struct {
	api      *gceapi.API
	fleet    string
	identity bool
}

func OpenTeardown(ctx context.Context, fleet string, s Settings) (*Teardown, error) {
	host, err := controllerVM(ctx, s.Zone)
	if err != nil {
		return nil, err
	}
	return &Teardown{api: host.api, fleet: fleet, identity: s.Identity == IdentityPerComputer}, nil
}

// TeardownPlan is what down removes (VMs) and keeps unless purged (home disks).
type TeardownPlan struct {
	VMs   []string // Computers
	Disks []TeardownDisk
}

type TeardownDisk struct {
	ID   string // HomeDriveID
	Name string // the disk
}

func (t *Teardown) Plan(ctx context.Context) (TeardownPlan, error) {
	var p TeardownPlan
	d := &Driver{fleet: t.fleet}
	list, err := t.api.ListInstances(ctx, d.computerFilter())
	if err != nil {
		return p, fmt.Errorf("list VMs: %w", err)
	}
	for _, in := range list {
		p.VMs = append(p.VMs, in.Labels[LabelComputer])
	}
	disks, err := t.api.ListDisks(ctx, "("+d.fleetFilter()+") (labels."+LabelDriveKind+"="+string(driver.DriveHome)+")")
	if err != nil {
		return p, fmt.Errorf("list disks: %w", err)
	}
	for _, disk := range disks {
		p.Disks = append(p.Disks, TeardownDisk{ID: driver.HomeDriveID(disk.Labels[LabelComputer]), Name: disk.Name})
	}
	slices.Sort(p.VMs)
	return p, nil
}

// Down deletes the plan's VMs (and their service accounts with per-computer identity), then with purge their home
// disks. Deleting a VM shuts it down, which stops its container.
func (t *Teardown) Down(ctx context.Context, p TeardownPlan, purge bool) (removed, destroyed []string, err error) {
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, computer := range p.VMs {
		wg.Go(func() {
			err := t.api.DeleteInstance(ctx, InstanceName(t.fleet, computer))
			if err == nil || gceapi.IsNotFound(err) {
				err = nil
				if t.identity {
					if derr := t.api.DeleteServiceAccount(ctx, ServiceAccountEmail(t.fleet, computer, t.api.Project)); derr != nil && !gceapi.IsNotFound(derr) {
						err = fmt.Errorf("service account of %s: %w", computer, derr)
					}
				}
			} else {
				err = fmt.Errorf("VM of %s: %w", computer, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else {
				removed = append(removed, computer)
			}
		})
	}
	wg.Wait()
	if purge && len(errs) == 0 {
		for _, disk := range p.Disks {
			if err := t.api.DeleteDisk(ctx, disk.Name); err != nil && !gceapi.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("disk %s: %w", disk.Name, err))
				continue
			}
			destroyed = append(destroyed, disk.Name)
		}
	}
	slices.Sort(removed)
	return removed, destroyed, errors.Join(errs...)
}
