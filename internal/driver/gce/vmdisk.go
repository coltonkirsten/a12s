package gce

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver/engine"
)

// A Computer VM's boot disk holds the images of the one Computer it runs and nothing else: every Materialize removes
// the images no container uses and the spec doesn't name, and checks there is room before it loads a new one.

// labelBundle names the bundle image (its ID on the Controller) the VM's bundle volume holds. The volume holds one
// bundle: a new one replaces it.
const labelBundle = "a12s.bundle"

// wantedImage is an image a spec names: its ID and size on the Controller, and its ID in this VM's engine if loaded.
type wantedImage struct {
	ref     string
	id      string
	size    int64
	localID string
}

func (g *vmGuest) wanted(ctx context.Context, ref string) (wantedImage, error) {
	id, err := g.e.Local.EnsureImage(ctx, ref)
	if err != nil {
		return wantedImage{}, err
	}
	w := wantedImage{ref: ref, id: id}
	res, err := g.e.Local.Client().ImageInspect(ctx, id)
	if err != nil {
		return wantedImage{}, fmt.Errorf("inspect %s: %w", ref, err)
	}
	w.size = res.Size
	local, err := g.eng.Client().ImageInspect(ctx, localImageRef(id))
	switch {
	case err == nil:
		w.localID = local.ID
	case !engine.IsNotFound(err):
		return wantedImage{}, fmt.Errorf("inspect %s: %w", localImageRef(id), err)
	}
	return w, nil
}

// pruneAround removes the images no container uses, keeping the copy of the Controller's image bundleID. It is
// best-effort: images left behind go at the next Materialize.
func (g *vmGuest) pruneAround(ctx context.Context, bundleID string) {
	res, err := g.eng.Client().ImageInspect(ctx, localImageRef(bundleID))
	switch {
	case err == nil:
		_ = g.prune(ctx, res.ID)
	case engine.IsNotFound(err):
		_ = g.prune(ctx)
	}
}

// prune removes every image no container uses, except keep. A removal that fails is skipped: the space check after
// it measures what is actually free.
func (g *vmGuest) prune(ctx context.Context, keep ...string) error {
	cli := g.eng.Client()
	cs, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	spare := map[string]bool{}
	for _, id := range keep {
		if id != "" {
			spare[id] = true
		}
	}
	for _, c := range cs.Items {
		spare[c.ImageID] = true
	}
	imgs, err := cli.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return fmt.Errorf("list images: %w", err)
	}
	for _, im := range imgs.Items {
		if spare[im.ID] {
			continue
		}
		// Force untags an image with several names; no container uses it, so nothing else is forced.
		_, _ = cli.ImageRemove(ctx, im.ID, client.ImageRemoveOptions{Force: true, PruneChildren: true})
	}
	return nil
}

// resetBundleVolume removes the bundle volume unless it holds bundleID, and reports whether a bundle must be
// extracted into a new one. No container may use the volume.
func (g *vmGuest) resetBundleVolume(ctx context.Context, bundleID string) (bool, error) {
	cli, name := g.eng.Client(), BundleVolume(g.e.Fleet)
	res, err := cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	switch {
	case engine.IsNotFound(err):
	case err != nil:
		return false, fmt.Errorf("inspect volume %s: %w", name, err)
	case res.Volume.Labels[labelBundle] == bundleID:
		return false, nil
	default:
		if _, err := cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil {
			return false, fmt.Errorf("remove volume %s: %w", name, err)
		}
	}
	labels := map[string]string{engine.LabelFleet: g.e.Fleet, labelBundle: bundleID}
	if _, err := cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels}); err != nil {
		return false, fmt.Errorf("create volume %s: %w", name, err)
	}
	return true, nil
}

// loadNeeds is the disk a load of these images takes at its peak: the engine unpacks the whole saved archive to a
// temporary directory before it registers the layers, so the largest image counts twice. extract is the bundle
// copied into the bundle volume.
func loadNeeds(load []wantedImage, extract int64) int64 {
	var sum, largest int64
	for _, w := range load {
		sum += w.size
		largest = max(largest, w.size)
	}
	return sum + largest + extract
}

// probeTimeout bounds the container that measures free space.
const probeTimeout = time.Minute

// freeBytes measures the space free for images in this engine by running df in a throwaway container of imageID: a
// container's root filesystem reports the engine's own. ok is false when the image can't tell (no df, say).
func (g *vmGuest) freeBytes(ctx context.Context, imageID string) (free int64, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cli, name := g.eng.Client(), g.containerName()+"-df"
	// A probe an interrupted Materialize left behind would hold the name.
	_, _ = cli.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:      imageID,
			User:       "0",
			Entrypoint: []string{"df", "-P", "-k", "/"},
			Labels:     map[string]string{engine.LabelFleet: g.e.Fleet},
		},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		return 0, false
	}
	defer cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return 0, false
	}
	wait := cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			return 0, false
		}
	case <-wait.Error:
		return 0, false
	}
	logs, err := cli.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true})
	if err != nil {
		return 0, false
	}
	defer logs.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, logs); err != nil {
		return 0, false
	}
	free, err = parseDF(out.String())
	return free, err == nil
}

// parseDF reads the available bytes from POSIX `df -P -k` output.
func parseDF(out string) (int64, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	var lines []string
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 2 {
		return 0, errors.New("df printed no filesystem")
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, fmt.Errorf("df line %q", lines[len(lines)-1])
	}
	kb, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("df available %q: %w", f[3], err)
	}
	return kb * 1024, nil
}

// NoRoomError is a VM whose boot disk can't hold the images its Computer needs, even holding nothing else.
type NoRoomError struct {
	VM     string
	Images []string
	Free   int64
	Needed int64
}

func (e *NoRoomError) Error() string {
	return fmt.Sprintf("no room on %s's boot disk for %s: %s free (%d bytes) after removing unused images, the load needs %s (%d bytes); "+
		"resize the boot disk or use a smaller image", e.VM, strings.Join(e.Images, " and "), gb(e.Free), e.Free, gb(e.Needed), e.Needed)
}

func gb(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

// makeRoom leaves only keep, and probe until it measured, on the VM, and fails with a NoRoomError when the images to
// load still don't fit. probe is an image already on the VM to measure with (the replaced generation's); "" skips
// the check, as on a new VM, which holds nothing to remove anyway.
func (g *vmGuest) makeRoom(ctx context.Context, want []wantedImage, extract int64, probe string) error {
	var keep []string
	var load []wantedImage
	for _, w := range want {
		keep = append(keep, w.localID)
		if w.localID == "" {
			load = append(load, w)
		}
	}
	needed := loadNeeds(load, extract)
	if needed == 0 {
		return g.prune(ctx, keep...)
	}
	if err := g.prune(ctx, append(keep, probe)...); err != nil {
		return err
	}
	if probe == "" {
		return nil
	}
	free, ok := g.freeBytes(ctx, probe)
	kept := false
	for _, id := range keep {
		kept = kept || id == probe
	}
	if !kept {
		if res, err := g.eng.Client().ImageInspect(ctx, probe); err == nil {
			free += res.Size
		}
		if err := g.prune(ctx, keep...); err != nil {
			return err
		}
	}
	if ok && free < needed {
		var refs []string
		for _, w := range load {
			refs = append(refs, w.ref)
		}
		return &NoRoomError{VM: g.vm.Name, Images: refs, Free: free, Needed: needed}
	}
	return nil
}
