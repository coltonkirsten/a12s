package docker

import (
	"context"
	"fmt"

	"github.com/coltonkirsten/a12s/internal/driver"
)

func (d *Driver) EnsureBundle(ctx context.Context, imageRef string) (driver.Bundle, error) {
	return d.eng.EnsureBundle(ctx, imageRef, d.BundleVolume())
}

func (d *Driver) EnsureImage(ctx context.Context, ref string) (string, error) {
	id, err := d.eng.EnsureImage(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("ensure image %s: %w", ref, err)
	}
	return id, nil
}

func (d *Driver) InspectComputerImage(ctx context.Context, imageID string) (driver.ComputerImage, error) {
	return d.eng.InspectComputerImage(ctx, imageID)
}
