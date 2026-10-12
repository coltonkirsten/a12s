package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
)

func (d *Driver) InspectComputerImage(ctx context.Context, imageID string) (driver.ComputerImage, error) {
	res, err := d.cli.ImageInspect(ctx, imageID)
	if err != nil {
		return driver.ComputerImage{}, fmt.Errorf("inspect image %s: %w", imageID, err)
	}
	var out driver.ComputerImage
	if res.Config != nil {
		out.Labels = res.Config.Labels
	}
	id, err := d.createExtractor(ctx, imageID, false)
	if err != nil {
		return driver.ComputerImage{}, fmt.Errorf("inspect image %s: %w", imageID, err)
	}
	defer d.removeExtractor(ctx, id)
	_, err = d.cli.ContainerStatPath(ctx, id, client.ContainerStatPathOptions{Path: driver.ComputerdMain})
	switch {
	case err == nil:
		out.Computerd = true
	case !isNotFound(err):
		return driver.ComputerImage{}, fmt.Errorf("inspect image %s: %w", imageID, err)
	}
	return out, nil
}
