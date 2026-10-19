package docker

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
)

// EnsureDrive never changes an existing volume, and refuses one this Fleet does not own.
func (d *Driver) EnsureDrive(ctx context.Context, spec driver.DriveSpec) (bool, error) {
	if spec.ID == "" {
		return false, fmt.Errorf("ensure drive: id is required")
	}
	name := d.VolumeName(spec.ID)
	labels := map[string]string{
		engine.LabelFleet: d.fleet,
		LabelDrive:        spec.ID,
	}
	if spec.Size > 0 {
		labels[LabelSize] = strconv.FormatInt(spec.Size, 10)
	}
	switch spec.Kind {
	case driver.DriveHome:
		labels[engine.LabelComputer] = spec.Computer
	case driver.DriveKeep:
		labels[engine.LabelComputer] = spec.Computer
		labels[LabelPath] = spec.Path
	case driver.DriveStateKind:
		labels[engine.LabelService] = spec.Computer
	}
	return d.eng.EnsureVolume(ctx, name, labels, LabelDrive, spec.ID)
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
	if engine.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove drive %s: %w", id, err)
	}
	if vol.Volume.Labels[engine.LabelFleet] != d.fleet || vol.Volume.Labels[LabelDrive] != id {
		return fmt.Errorf("remove drive %s: volume %s is not owned by fleet %s", id, name, d.fleet)
	}
	if _, err := d.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil && !engine.IsNotFound(err) {
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
