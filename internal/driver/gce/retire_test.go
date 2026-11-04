package gce

import (
	"errors"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/driver"
)

func TestRetireHome(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	id, disk := driver.HomeDriveID("scout"), DiskName("lab", "home-scout")
	until := time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC)
	if _, err := h.d.RetireDrive(h.ctx, id, until); !errors.Is(err, driver.ErrDriveAttached) {
		t.Fatalf("retire an attached home = %v, want ErrDriveAttached", err)
	}
	if _, ok := h.fake.Disk(disk); !ok {
		t.Fatal("an attached home was deleted")
	}
	h.must(h.d.RemoveComputer(h.ctx, "scout"))
	d0, _ := h.fake.Disk(disk)

	h.fake.FailOp("createSnapshot", disk, "INTERNAL_ERROR")
	if _, err := h.d.RetireDrive(h.ctx, id, until); err == nil {
		t.Fatal("a failed snapshot retired the home")
	}
	if _, ok := h.fake.Disk(disk); !ok {
		t.Fatal("the disk was deleted without a snapshot")
	}

	snap, err := h.d.RetireDrive(h.ctx, id, until)
	h.must(err)
	if want := SnapshotName(disk, d0.ID); snap != want {
		t.Fatalf("snapshot = %q, want %q", snap, want)
	}
	s, ok := h.fake.Snapshot(snap)
	if !ok || s.Labels[LabelComputer] != "scout" || s.Labels[LabelExpires] != "1796256000" {
		t.Fatalf("snapshot = %+v, %v", s, ok)
	}
	if _, ok := h.fake.Disk(disk); ok {
		t.Fatal("the disk stayed after its snapshot")
	}
	if again, err := h.d.RetireDrive(h.ctx, id, until); err != nil || again != "" {
		t.Fatalf("retire again = %q, %v; want nothing to do", again, err)
	}

	if created, err := h.d.EnsureDrive(h.ctx, driver.DriveSpec{ID: id, Kind: driver.DriveHome, Computer: "scout"}); err != nil || !created {
		t.Fatalf("EnsureDrive after retiring = %v, %v", created, err)
	}
	if d1, _ := h.fake.Disk(disk); last(d1.SourceSnapshot) != snap {
		t.Fatalf("the home came back from %q, want snapshot %s", d1.SourceSnapshot, snap)
	}

	if got, err := h.d.ExpireRetired(h.ctx, until.Add(-time.Second)); err != nil || len(got) != 0 {
		t.Fatalf("expire before the expiry = %v, %v", got, err)
	}
	if got, err := h.d.ExpireRetired(h.ctx, until); err != nil || len(got) != 1 || got[0] != snap {
		t.Fatalf("expire at the expiry = %v, %v", got, err)
	}
	if _, ok := h.fake.Snapshot(snap); ok {
		t.Fatal("an expired snapshot stayed")
	}
}
