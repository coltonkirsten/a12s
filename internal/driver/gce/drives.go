package gce

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

const gib = 1 << 30

// EnsureDrive creates a home drive as a zonal persistent disk, or grows an existing one. GCE can't shrink a disk;
// a lower Size changes nothing, as on Docker. The VM grows its filesystem when it next boots.
func (d *Driver) EnsureDrive(ctx context.Context, spec driver.DriveSpec) (bool, error) {
	if spec.ID == "" {
		return false, errors.New("ensure drive: id is required")
	}
	if spec.Kind != driver.DriveHome || spec.ID != driver.HomeDriveID(spec.Computer) {
		return false, fmt.Errorf("ensure drive %s: the GCE driver has only home drives", spec.ID)
	}
	sizeGb := int64(DefaultHomeDiskGB)
	if spec.Size > 0 {
		sizeGb = max((spec.Size+gib-1)/gib, 10)
	}
	name := d.diskName(spec.ID)
	disk, err := d.api.GetDisk(ctx, name)
	switch {
	case gceapi.IsNotFound(err):
	case err != nil:
		return false, fmt.Errorf("ensure drive %s: %w", spec.ID, err)
	default:
		if disk.Labels[LabelFleet] != d.fleet || disk.Labels[LabelComputer] != spec.Computer || disk.Labels[LabelDriveKind] != string(driver.DriveHome) {
			return false, fmt.Errorf("ensure drive %s: disk %s exists but is not owned by fleet %s", spec.ID, name, d.fleet)
		}
		if spec.Size > 0 && sizeGb > disk.SizeGb {
			if err := d.api.ResizeDisk(ctx, name, sizeGb); err != nil {
				return false, fmt.Errorf("ensure drive %s: grow to %d GiB: %w", spec.ID, sizeGb, err)
			}
		}
		return false, nil
	}
	disk = &gceapi.Disk{
		Name:        name,
		Description: "a12s home drive " + d.fleet + "/" + spec.Computer,
		SizeGb:      sizeGb,
		Type:        d.api.ZoneURL("diskTypes", DiskType),
		Labels:      map[string]string{LabelFleet: d.fleet, LabelComputer: spec.Computer, LabelDriveKind: string(driver.DriveHome)},
	}
	if d.cfg.SnapshotPolicy != "" {
		disk.ResourcePolicies = []string{d.api.RegionURL("resourcePolicies", d.cfg.SnapshotPolicy)}
	}
	err = d.api.InsertDisk(ctx, disk)
	if gceapi.IsConflict(err) {
		return d.EnsureDrive(ctx, spec) // created concurrently: check that it is ours
	}
	if err != nil {
		return false, fmt.Errorf("ensure drive %s: %w", spec.ID, err)
	}
	return true, nil
}

// RemoveDrive refuses while any VM, a12s or not, has the disk attached. Snapshots stay under their policy's retention.
func (d *Driver) RemoveDrive(ctx context.Context, id string) error {
	name := d.diskName(id)
	disk, err := d.api.GetDisk(ctx, name)
	if gceapi.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove drive %s: %w", id, err)
	}
	if disk.Labels[LabelFleet] != d.fleet || driver.HomeDriveID(disk.Labels[LabelComputer]) != id {
		return fmt.Errorf("remove drive %s: disk %s is not owned by fleet %s", id, name, d.fleet)
	}
	if len(disk.Users) > 0 {
		var users []string
		for _, u := range disk.Users {
			users = append(users, last(u))
		}
		return fmt.Errorf("remove drive %s: %w to %s", id, driver.ErrDriveAttached, strings.Join(users, ", "))
	}
	if err := d.api.DeleteDisk(ctx, name); err != nil && !gceapi.IsNotFound(err) {
		if gceapi.HasReason(err, "resourceInUseByAnotherResource") {
			return fmt.Errorf("remove drive %s: %w", id, driver.ErrDriveAttached)
		}
		return fmt.Errorf("remove drive %s: %w", id, err)
	}
	return nil
}
