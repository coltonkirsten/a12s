package docker

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// EnsureDrive never changes an existing volume, and refuses one this Fleet does not own.
func (d *Driver) EnsureDrive(ctx context.Context, spec driver.DriveSpec) (bool, error) {
	if spec.ID == "" {
		return false, fmt.Errorf("ensure drive: id is required")
	}
	name := d.VolumeName(spec.ID)
	labels := map[string]string{
		LabelFleet: d.fleet,
		LabelDrive: spec.ID,
	}
	if spec.Size > 0 {
		labels[LabelSize] = strconv.FormatInt(spec.Size, 10)
	}
	switch spec.Kind {
	case driver.DriveHome:
		labels[LabelComputer] = spec.Computer
	case driver.DriveKeep:
		labels[LabelComputer] = spec.Computer
		labels[LabelPath] = spec.Path
	case driver.DriveStateKind:
		labels[LabelService] = spec.Computer
	}
	return d.ensureVolume(ctx, name, labels, LabelDrive, spec.ID)
}

// ensureVolume refuses an existing volume not labeled for this Fleet and labels[key] == want.
func (d *Driver) ensureVolume(ctx context.Context, name string, labels map[string]string, key, want string) (bool, error) {
	res, err := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err == nil {
		got := res.Volume.Labels
		if got[LabelFleet] != d.fleet || got[key] != want {
			return false, fmt.Errorf("volume %s exists but is not owned by fleet %s", name, d.fleet)
		}
		return false, nil
	}
	if !isNotFound(err) {
		return false, fmt.Errorf("inspect volume %s: %w", name, err)
	}
	if _, err := d.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels}); err != nil {
		return false, fmt.Errorf("create volume %s: %w", name, err)
	}
	return true, nil
}

// RemoveDrive refuses while any container, a12s or not, has the volume mounted.
func (d *Driver) RemoveDrive(ctx context.Context, id string) error {
	name := d.VolumeName(id)
	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("volume", name),
	})
	if err != nil {
		return fmt.Errorf("remove drive %s: %w", id, err)
	}
	if len(res.Items) > 0 {
		var users []string
		for _, c := range res.Items {
			users = append(users, strings.TrimPrefix(firstName(c.Names), "/"))
		}
		return fmt.Errorf("remove drive %s: %w to %s", id, driver.ErrDriveAttached, strings.Join(users, ", "))
	}
	vol, err := d.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove drive %s: %w", id, err)
	}
	if vol.Volume.Labels[LabelFleet] != d.fleet || vol.Volume.Labels[LabelDrive] != id {
		return fmt.Errorf("remove drive %s: volume %s is not owned by fleet %s", id, name, d.fleet)
	}
	if _, err := d.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove drive %s: %w", id, err)
	}
	return nil
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}
