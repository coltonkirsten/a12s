package cli

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/version"
)

// FleetImages is every image ref `up` needs: the Controller, Computer images, harness bundles and Services.
func FleetImages(f *fleet.Fleet, controllerImage string) []string {
	seen := map[string]bool{}
	var refs []string
	add := func(r string) {
		if r != "" && !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	add(controllerImage)
	for _, name := range f.ComputerNames() {
		c := f.Computers[name]
		switch c.Image {
		case fleet.TierSlim, fleet.TierBrowser:
			add(version.Image(c.Image))
		default:
			add(c.Image)
		}
		add(version.Bundle(c.Harness.Kind, c.Harness.Version))
	}
	for _, name := range f.ServiceNames() {
		add(f.Services[name].Image)
	}
	return refs
}

// PullMissing pulls absent images on the host with the docker CLI. The a12s images on ghcr.io are public; the
// Controller's Engine API pull carries no registry credentials, so a private registry of the user's own only works
// through their `docker login`.
// Failures are reported and left for the Controller to turn into an admission error with the field name.
func PullMissing(ctx context.Context, refs []string, log io.Writer) {
	dc, err := client.New(client.FromEnv)
	if err != nil {
		return
	}
	defer dc.Close()
	docker, err := exec.LookPath("docker")
	if err != nil {
		return
	}
	for _, ref := range refs {
		if _, err := dc.ImageInspect(ctx, ref); err == nil {
			continue
		}
		if !version.Release() && strings.HasPrefix(ref, version.Registry+"/") {
			continue // a source build's local images come from make, not a registry
		}
		fmt.Fprintf(log, "pulling %s\n", ref)
		cmd := exec.CommandContext(ctx, docker, "pull", "--quiet", ref)
		cmd.Stdout, cmd.Stderr = io.Discard, log
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(log, "could not pull %s (%v)\n", ref, err)
		}
	}
}
