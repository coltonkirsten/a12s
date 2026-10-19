// Package drivertest is what every driver.Driver owes the Controller, written against the interface alone: the
// Docker driver runs it against Docker, the GCE driver against its fake API, so a Fleet behaves the same on both.
package drivertest

import (
	"context"
	"errors"
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver"
)

type Setup struct {
	Driver driver.Driver
	// Spec is a stageable spec of name at generation gen: an image and bundle the substrate has.
	Spec func(name string, gen int64) driver.ComputerSpec
}

// Run takes one Computer, c, through its life.
func Run(t *testing.T, s Setup) {
	ctx := context.Background()
	d := s.Driver
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	want := func(what string, err, target error) {
		t.Helper()
		if !errors.Is(err, target) {
			t.Errorf("%s = %v, want %v", what, err, target)
		}
	}
	state := func() (driver.ComputerState, driver.Observation) {
		t.Helper()
		obs, err := d.Observe(ctx)
		must(err)
		return obs.Computers["c"], obs
	}

	home := driver.DriveSpec{ID: driver.HomeDriveID("c"), Kind: driver.DriveHome, Computer: "c"}
	if created, err := d.EnsureDrive(ctx, home); err != nil || !created {
		t.Fatalf("EnsureDrive = %v, %v; want created", created, err)
	}
	if created, err := d.EnsureDrive(ctx, home); err != nil || created {
		t.Fatalf("EnsureDrive again = %v, %v; want existing", created, err)
	}

	must(d.StageComputer(ctx, s.Spec("c", 1)))
	must(d.StageComputer(ctx, s.Spec("c", 1)))
	if st, obs := state(); st.Name != "c" || st.Generation != 1 || st.Running || st.Power != driver.PowerAsleep {
		t.Fatalf("staged: %+v", st)
	} else if dr, ok := obs.Drives[home.ID]; !ok || len(dr.AttachedTo) != 1 || dr.AttachedTo[0] != "c" {
		t.Errorf("home drive after staging: %+v, %v", dr, ok)
	}

	want("SetPower of an older generation", d.SetPower(ctx, "c", driver.PowerAwake, 0), driver.ErrStaleGeneration)
	want("SetPower of an unstaged generation", d.SetPower(ctx, "c", driver.PowerAwake, 2), driver.ErrNotStaged)
	must(d.SetPower(ctx, "c", driver.PowerAwake, 1))
	must(d.SetPower(ctx, "c", driver.PowerAwake, 1))
	if st, _ := state(); !st.Running || st.Power != driver.PowerAwake || st.Generation != 1 || st.Address == "" {
		t.Fatalf("awake: %+v", st)
	}
	if code, _, err := d.Exec(ctx, "c", []string{"true"}, nil, false); err != nil || code != 0 {
		t.Errorf("Exec true = %d, %v", code, err)
	}
	want("StageComputer over a running Computer", d.StageComputer(ctx, s.Spec("c", 2)), driver.ErrRunning)
	want("RemoveDrive of an attached drive", d.RemoveDrive(ctx, home.ID), driver.ErrDriveAttached)

	must(d.SetPower(ctx, "c", driver.PowerAsleep, 1))
	must(d.SetPower(ctx, "c", driver.PowerAsleep, 1))
	if st, _ := state(); st.Running || st.Power != driver.PowerAsleep || st.Failure != "" {
		t.Fatalf("asleep: %+v", st)
	}
	if n, err := d.Sessions(ctx, "c"); err != nil || n != 0 {
		t.Errorf("Sessions asleep = %d, %v", n, err)
	}

	must(d.StageComputer(ctx, s.Spec("c", 2)))
	want("StageComputer of an older generation", d.StageComputer(ctx, s.Spec("c", 1)), driver.ErrStaleGeneration)
	want("SetPower of the replaced generation", d.SetPower(ctx, "c", driver.PowerAwake, 1), driver.ErrStaleGeneration)
	must(d.SetPower(ctx, "c", driver.PowerAwake, 2))
	if st, _ := state(); !st.Running || st.Generation != 2 {
		t.Fatalf("awake after a restage: %+v", st)
	}
	must(d.SetPower(ctx, "c", driver.PowerAsleep, 2))

	must(d.RemoveComputer(ctx, "c"))
	must(d.RemoveComputer(ctx, "c"))
	st, obs := state()
	if _, ok := obs.Computers["c"]; ok {
		t.Fatalf("removed Computer still observed: %+v", st)
	}
	if dr, ok := obs.Drives[home.ID]; !ok || len(dr.AttachedTo) != 0 {
		t.Fatalf("home drive after removal: %+v, %v (drives are kept)", dr, ok)
	}
	want("SetPower of a removed Computer", d.SetPower(ctx, "c", driver.PowerAwake, 2), driver.ErrNotStaged)
	must(d.RemoveDrive(ctx, home.ID))
	must(d.RemoveDrive(ctx, home.ID))
	if _, obs := state(); len(obs.Drives) != 0 {
		t.Fatalf("drives after RemoveDrive: %+v", obs.Drives)
	}
}
