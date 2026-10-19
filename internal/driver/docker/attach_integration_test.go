package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// An attached folder is the host folder itself, both ways, read-only when asked; a kept path lives on its own drive
// and never reaches the host; a missing folder fails with the fix.
func TestDockerAttachIntegration(t *testing.T) {
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

	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	app, docs := filepath.Join(root, "app"), filepath.Join(root, "docs")
	must(t, os.MkdirAll(app, 0o755))
	must(t, os.MkdirAll(docs, 0o755))
	must(t, os.WriteFile(filepath.Join(app, "README.md"), []byte("from the host\n"), 0o644))

	keep := driver.KeepDriveID("c", "project", "node_modules")
	spec := driver.ComputerSpec{
		Name: "c", Generation: 1, Image: standin, Tier: fleet.TierCustom,
		Bundle:    driver.BundleRef{Kind: "codex", Version: "0.0.0-test", Image: b.ImageID},
		HomeDrive: driver.HomeDriveID("c"),
		Attach: []driver.Attach{
			{Host: docs, Target: "/home/agent/docs", ReadOnly: true},
			{Host: app, Target: "/home/agent/project", Keep: []driver.Keep{{Drive: keep, Target: "/home/agent/project/node_modules"}}},
		},
	}
	_, err = d.EnsureDrive(ctx, driver.DriveSpec{ID: spec.HomeDrive, Kind: driver.DriveHome, Computer: "c"})
	must(t, err)
	_, err = d.EnsureDrive(ctx, driver.DriveSpec{ID: keep, Kind: driver.DriveKeep, Computer: "c", Path: "/home/agent/project/node_modules"})
	must(t, err)
	must(t, d.StageComputer(ctx, spec))
	must(t, d.SetPower(ctx, "c", driver.PowerAwake, 1))

	sh := func(script string) (int, string) {
		t.Helper()
		code, out, err := d.Exec(ctx, "c", []string{"sh", "-c", script}, nil, true)
		must(t, err)
		return code, out
	}
	if _, out := sh("cat /home/agent/project/README.md"); out != "from the host\n" {
		t.Errorf("host file in the container = %q", out)
	}
	if code, out := sh("echo from the container > /home/agent/project/out.txt"); code != 0 {
		t.Fatalf("write = %d %s", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(app, "out.txt")); err != nil || string(b) != "from the container\n" {
		t.Errorf("container file on the host = %q, %v", b, err)
	}
	must(t, os.WriteFile(filepath.Join(app, "README.md"), []byte("edited\n"), 0o644))
	if _, out := sh("cat /home/agent/project/README.md"); out != "edited\n" {
		t.Errorf("host edit in the container = %q", out)
	}
	if code, out := sh("touch /home/agent/docs/x"); code == 0 || !strings.Contains(out, "Read-only") {
		t.Errorf("write to a read-only folder = %d %q", code, out)
	}
	if code, out := sh("echo pkg > /home/agent/project/node_modules/left-pad"); code != 0 {
		t.Fatalf("write to the kept path = %d %s", code, out)
	}
	if entries, err := os.ReadDir(filepath.Join(app, "node_modules")); err != nil || len(entries) != 0 {
		t.Errorf("the kept path reached the host folder: %v %v", entries, err)
	}
	obs, err := d.Observe(ctx)
	must(t, err)
	if ds := obs.Drives[keep]; ds.Computer != "c" || ds.Path != "/home/agent/project/node_modules" || len(ds.AttachedTo) != 1 {
		t.Errorf("keep drive = %+v", ds)
	}

	// The kept path outlives a restage, like the home drive.
	must(t, d.SetPower(ctx, "c", driver.PowerAsleep, 1))
	spec.Generation = 2
	must(t, d.StageComputer(ctx, spec))
	must(t, d.SetPower(ctx, "c", driver.PowerAwake, 2))
	if _, out := sh("cat /home/agent/project/node_modules/left-pad"); out != "pkg\n" {
		t.Errorf("kept path after restage = %q", out)
	}

	// A folder that is gone fails the start with the fix, naming the folder.
	must(t, d.SetPower(ctx, "c", driver.PowerAsleep, 2))
	must(t, os.RemoveAll(docs))
	err = d.SetPower(ctx, "c", driver.PowerAwake, 2)
	if err == nil || !strings.Contains(err.Error(), "Docker can't see attach.docs ("+docs+")") {
		t.Errorf("start with a missing folder = %v", err)
	}
	spec.Generation = 3
	err = d.StageComputer(ctx, spec)
	if err == nil || !strings.Contains(err.Error(), "Docker can't see attach.docs ("+docs+")") {
		t.Errorf("stage with a missing folder = %v", err)
	}
}
