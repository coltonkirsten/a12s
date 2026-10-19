package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/drivertest"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

func TestDockerConformance(t *testing.T) {
	if os.Getenv("A12S_DOCKER_TEST") != "1" {
		t.Skip("set A12S_DOCKER_TEST=1 to run against Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	fleetName := "test-" + hex.EncodeToString(suffix)
	cli, err := client.New(client.FromEnv)
	must(t, err)
	d, err := New(Config{Fleet: fleetName, Client: cli})
	must(t, err)
	standin := "a12s-test-standin:" + fleetName
	bundleRef := "a12s-test-bundle-codex:" + fleetName
	t.Cleanup(func() { cleanup(t, cli, fleetName, standin, bundleRef) })
	buildImage(ctx, t, cli, standin, map[string]string{"Dockerfile": standinDockerfile})
	buildImage(ctx, t, cli, bundleRef, map[string]string{
		"Dockerfile":         bundleDockerfile,
		"bundle/bundle.json": bundleJSON,
		"bundle/bin/codex":   "#!/bin/sh\necho fake codex\n",
	})
	startStandinController(ctx, t, cli, d, fleetName)
	b, err := d.EnsureBundle(ctx, bundleRef)
	must(t, err)
	drivertest.Run(t, drivertest.Setup{Driver: d, Spec: func(name string, gen int64) driver.ComputerSpec {
		return driver.ComputerSpec{
			Name: name, Generation: gen, Image: standin, Tier: fleet.TierCustom,
			Bundle:    driver.BundleRef{Kind: "codex", Version: "0.0.0-test", Image: b.ImageID},
			HomeDrive: driver.HomeDriveID(name),
		}
	}})
}
