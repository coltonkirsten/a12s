package gce

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
)

const gib = 1 << 30

// EnsureDrive creates a home drive as a zonal persistent disk, from the Computer's newest retired snapshot if it has
// one, or grows an existing one. GCE can't shrink a disk;
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
	snap, err := d.newestRetired(ctx, spec.Computer)
	if err != nil {
		return false, fmt.Errorf("ensure drive %s: %w", spec.ID, err)
	}
	disk = &gceapi.Disk{
		Name:        name,
		Description: "a12s home drive " + d.fleet + "/" + spec.Computer,
		SizeGb:      sizeGb,
		Type:        d.api.ZoneURL("diskTypes", DiskType),
		Labels:      map[string]string{LabelFleet: d.fleet, LabelComputer: spec.Computer, LabelDriveKind: string(driver.DriveHome)},
	}
	if snap != nil {
		disk.SourceSnapshot = d.api.GlobalURL("snapshots", snap.Name)
		disk.SizeGb = max(disk.SizeGb, snap.DiskSizeGb)
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

// RemoveDrive refuses while any VM, a12s or not, has the disk attached. Snapshots stay: a policy's under its
// retention, a retired home's until it expires.
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

func (d *Driver) retiredFilter() string {
	return "(labels." + LabelFleet + "=" + d.fleet + ") (labels." + LabelDriveKind + "=" + string(driver.DriveHome) + ")"
}

func (d *Driver) newestRetired(ctx context.Context, computer string) (*gceapi.Snapshot, error) {
	list, err := d.api.ListSnapshots(ctx, d.retiredFilter()+" (labels."+LabelComputer+"="+computer+")")
	if err != nil {
		return nil, fmt.Errorf("list retired snapshots: %w", err)
	}
	var newest *gceapi.Snapshot
	for i := range list {
		s := &list[i]
		if s.Status != "READY" || s.Labels[LabelExpires] == "" {
			continue
		}
		if newest == nil || s.CreationTimestamp > newest.CreationTimestamp {
			newest = s
		}
	}
	return newest, nil
}

// RetireDrive deletes the disk only after its snapshot is READY. A VM attaching the disk in between makes the
// delete fail, so a disk in use is never deleted.
func (d *Driver) RetireDrive(ctx context.Context, id string, until time.Time) (string, error) {
	computer, ok := strings.CutPrefix(id, "home-")
	if !ok || driver.HomeDriveID(computer) != id {
		return "", fmt.Errorf("retire drive %s: only home drives retire", id)
	}
	defer d.lock(computer)()
	name := d.diskName(id)
	disk, err := d.api.GetDisk(ctx, name)
	if gceapi.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("retire drive %s: %w", id, err)
	}
	if disk.Labels[LabelFleet] != d.fleet || disk.Labels[LabelComputer] != computer || disk.Labels[LabelDriveKind] != string(driver.DriveHome) {
		return "", fmt.Errorf("retire drive %s: disk %s is not owned by fleet %s", id, name, d.fleet)
	}
	if len(disk.Users) > 0 {
		var users []string
		for _, u := range disk.Users {
			users = append(users, last(u))
		}
		return "", fmt.Errorf("retire drive %s: %w to %s", id, driver.ErrDriveAttached, strings.Join(users, ", "))
	}
	snapName := SnapshotName(name, disk.ID)
	snap, err := d.api.GetSnapshot(ctx, snapName)
	if gceapi.IsNotFound(err) {
		err = d.api.CreateSnapshot(ctx, name, &gceapi.Snapshot{
			Name:        snapName,
			Description: "a12s retired home drive " + d.fleet + "/" + computer,
			Labels: map[string]string{
				LabelFleet: d.fleet, LabelComputer: computer, LabelDriveKind: string(driver.DriveHome),
				LabelExpires: strconv.FormatInt(until.Unix(), 10),
			},
		})
		if err != nil && !gceapi.IsConflict(err) {
			return "", fmt.Errorf("retire drive %s: snapshot: %w", id, err)
		}
		snap, err = d.api.GetSnapshot(ctx, snapName)
	}
	if err != nil {
		return "", fmt.Errorf("retire drive %s: snapshot %s: %w", id, snapName, err)
	}
	if snap.Status != "READY" {
		return "", fmt.Errorf("retire drive %s: snapshot %s is %s, so the disk stays", id, snapName, snap.Status)
	}
	if err := d.api.DeleteDisk(ctx, name); err != nil && !gceapi.IsNotFound(err) {
		if gceapi.HasReason(err, "resourceInUseByAnotherResource") {
			return "", fmt.Errorf("retire drive %s: %w", id, driver.ErrDriveAttached)
		}
		return "", fmt.Errorf("retire drive %s: delete disk: %w", id, err)
	}
	return snapName, nil
}

func (d *Driver) ExpireRetired(ctx context.Context, now time.Time) ([]string, error) {
	list, err := d.api.ListSnapshots(ctx, d.retiredFilter())
	if err != nil {
		return nil, fmt.Errorf("expire retired snapshots: %w", err)
	}
	var deleted []string
	var errs []error
	for _, s := range list {
		at, err := strconv.ParseInt(s.Labels[LabelExpires], 10, 64)
		if err != nil || now.Before(time.Unix(at, 0)) {
			continue
		}
		if err := d.api.DeleteSnapshot(ctx, s.Name); err != nil && !gceapi.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete snapshot %s: %w", s.Name, err))
			continue
		}
		deleted = append(deleted, s.Name)
	}
	return deleted, errors.Join(errs...)
}
