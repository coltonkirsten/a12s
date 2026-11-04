package controller

import (
	"context"
	"errors"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
)

// homeRetention is how long a removed agent computer's home stays restorable on a Driver that retires homes.
const homeRetention = 30 * 24 * time.Hour

// retirementLoop retires orphaned home drives (removals that could not retire theirs, or that predate retiring) and
// deletes expired retired snapshots, hourly.
func (c *Controller) retirementLoop(ctx context.Context) {
	r, ok := c.drv.(driver.HomeRetirer)
	if !ok {
		return
	}
	for {
		c.applyMu.Lock()
		c.retireOrphanHomes(ctx, r)
		c.applyMu.Unlock()
		expired, err := r.ExpireRetired(ctx, time.Now())
		for _, s := range expired {
			c.logf("retired home snapshot %s expired: deleted", s)
		}
		if err != nil && ctx.Err() == nil {
			c.logf("retired home expiry: %v", err)
		}
		t := time.NewTimer(time.Hour)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// retireOrphanHomes needs applyMu held exclusively, so no apply declares a Computer while its home is retired.
func (c *Controller) retireOrphanHomes(ctx context.Context, r driver.HomeRetirer) {
	c.mu.Lock()
	applied := c.state.Fleet != nil
	c.mu.Unlock()
	if !applied { // before the first apply every home would look orphaned
		return
	}
	rows, err := c.drives(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.logf("retire orphaned homes: %v", err)
		}
		return
	}
	for _, row := range rows {
		if row.Kind == api.DriveHome && row.Orphaned && len(row.AttachedTo) == 0 {
			c.retireHome(ctx, r, row.Computer)
		}
	}
}

// retireHome runs only for a Computer the Fleet no longer declares; a failure keeps the drive for the next pass.
func (c *Controller) retireHome(ctx context.Context, r driver.HomeRetirer, name string) {
	until := time.Now().Add(homeRetention).UTC()
	snap, err := r.RetireDrive(ctx, driver.HomeDriveID(name), until)
	switch {
	case errors.Is(err, driver.ErrDriveAttached):
		c.logf("%s: home drive kept: %v", name, err)
	case err != nil:
		c.logf("%s: home drive kept, retried hourly: %v", name, err)
	case snap != "":
		c.logf("%s: home drive deleted; snapshot %s restores it if %s is added again until %s", name, snap, name, until.Format(time.RFC3339))
	}
}
