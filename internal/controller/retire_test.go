package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// retiringDriver retires a home by deleting the drive, as the GCE driver does after its snapshot.
type retiringDriver struct {
	*fakeDriver
}

func (r retiringDriver) RetireDrive(ctx context.Context, id string, _ time.Time) (string, error) {
	if !r.hasDrive(id) {
		return "", nil
	}
	if err := r.RemoveDrive(ctx, id); err != nil {
		return "", err
	}
	r.record("retire %s", id)
	return "snap-" + id, nil
}

func (r retiringDriver) ExpireRetired(context.Context, time.Time) ([]string, error) { return nil, nil }

func TestRemovalRetiresTheHomeAndTheSweepRetiresOrphans(t *testing.T) {
	h := startHarnessOn(t, t.TempDir(), nil, func(d *fakeDriver) driver.Driver { return retiringDriver{d} }, 1)
	h.up(labFleet(t, fifth), false)
	h.d.takeCalls()

	h.up(labFleet(t, nil), false)
	if calls := h.d.takeCalls(); !slices.Equal(calls, []string{"remove fifth", "rm drive home-fifth", "retire home-fifth"}) {
		t.Fatalf("calls = %v", calls)
	}

	_, err := h.d.EnsureDrive(context.Background(), driver.DriveSpec{ID: "home-left", Kind: driver.DriveHome, Computer: "left"})
	if err != nil {
		t.Fatal(err)
	}
	h.d.takeCalls()
	h.c.applyMu.Lock()
	h.c.retireOrphanHomes(context.Background(), retiringDriver{h.d})
	h.c.applyMu.Unlock()
	if calls := h.d.takeCalls(); !slices.Equal(calls, []string{"rm drive home-left", "retire home-left"}) {
		t.Fatalf("the sweep called %v; it must retire only the orphan", calls)
	}
	for _, name := range labFleet(t, nil).ComputerNames() {
		if !h.d.hasDrive(driver.HomeDriveID(name)) {
			t.Fatalf("the home of declared %s was retired", name)
		}
	}
}
