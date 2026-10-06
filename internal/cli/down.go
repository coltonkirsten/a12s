package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver/docker"
)

// Down is the CLI's side of `a12s down`. It talks to Docker directly, the
// way EnsureController does, rather than asking the Controller: the
// Controller cannot remove its own container, network and state volume
// while it answers the request, and down must also work when the Controller
// is stopped, crashed, or already gone.
//
// Only objects labeled a12s.fleet=<fleet> are touched, and each is checked
// again before it is removed. Drives (volumes labeled a12s.drive) are kept
// unless purge is set; every other a12s volume of the Fleet (Controller
// state with the CA, the bundle cache) is removed. In ~/.a12s/<fleet> only
// the CLI's own controller.json is removed.

// DownDocker is the part of the Docker client that Down uses.
type DownDocker interface {
	ContainerList(ctx context.Context, o client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerStop(ctx context.Context, id string, o client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	NetworkList(ctx context.Context, o client.NetworkListOptions) (client.NetworkListResult, error)
	NetworkInspect(ctx context.Context, id string, o client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkDisconnect(ctx context.Context, id string, o client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error)
	NetworkRemove(ctx context.Context, id string, o client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
	VolumeInspect(ctx context.Context, id string, o client.VolumeInspectOptions) (client.VolumeInspectResult, error)
	VolumeList(ctx context.Context, o client.VolumeListOptions) (client.VolumeListResult, error)
	VolumeRemove(ctx context.Context, id string, o client.VolumeRemoveOptions) (client.VolumeRemoveResult, error)
}

var _ DownDocker = (*client.Client)(nil)

// Drive is one drive volume of a Fleet, as `a12s drive ls` names it.
type Drive struct {
	ID     string // the a12s.drive label: "home-<computer>" or "drive-<name>"
	Name   string // "home-<computer>" or the work drive's name
	Volume string // the Docker volume
}

// DownPlan is what `a12s down` found for a Fleet.
type DownPlan struct {
	Fleet      string
	Containers []container.Summary // Controller first, then Computers, then anything else
	Networks   []string
	Volumes    []string // non-drive volumes: Controller state, bundle cache
	Drives     []Drive
	StateDir   string   // ~/.a12s/<fleet>, "" when absent or not a directory
	StateFiles []string // the CLI's own files in it (controller.json)
}

// Empty reports whether nothing of the Fleet exists, drives aside.
func (p *DownPlan) Empty() bool {
	return len(p.Containers) == 0 && len(p.Networks) == 0 && len(p.Volumes) == 0 && len(p.StateFiles) == 0
}

// DownResult is what Down did.
type DownResult struct {
	Containers []string // names
	Networks   []string
	Volumes    []string
	StateFiles []string // removed from the state dir
	StateDir   string   // removed (it was empty after StateFiles), or ""
	Kept       []Drive  // drives left in place
	Destroyed  []Drive  // drives removed (purge only)
}

func fleetFilter(fleetName string) client.Filters {
	return make(client.Filters).Add("label", docker.LabelFleet+"="+fleetName)
}

// PlanDown lists the Fleet's Docker objects and state directory.
func PlanDown(ctx context.Context, dc DownDocker, fleetName string) (*DownPlan, error) {
	if fleetName == "" {
		return nil, errors.New("down: fleet name is required")
	}
	p := &DownPlan{Fleet: fleetName}
	cl, err := dc.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: fleetFilter(fleetName)})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	for _, c := range cl.Items {
		if c.Labels[docker.LabelFleet] == fleetName {
			p.Containers = append(p.Containers, c)
		}
	}
	rank := func(c container.Summary) int {
		switch c.Labels[docker.LabelRole] {
		case docker.RoleController:
			return 0
		case docker.RoleComputer:
			return 1
		}
		return 2
	}
	sort.SliceStable(p.Containers, func(i, j int) bool {
		if ri, rj := rank(p.Containers[i]), rank(p.Containers[j]); ri != rj {
			return ri < rj
		}
		return containerName(p.Containers[i]) < containerName(p.Containers[j])
	})

	nl, err := dc.NetworkList(ctx, client.NetworkListOptions{Filters: fleetFilter(fleetName)})
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	for _, n := range nl.Items {
		if n.Labels[docker.LabelFleet] == fleetName {
			p.Networks = append(p.Networks, n.Name)
		}
	}
	sort.Strings(p.Networks)

	vl, err := dc.VolumeList(ctx, client.VolumeListOptions{Filters: fleetFilter(fleetName)})
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	for _, v := range vl.Items {
		if v.Labels[docker.LabelFleet] != fleetName {
			continue
		}
		if id := v.Labels[docker.LabelDrive]; id != "" {
			p.Drives = append(p.Drives, Drive{ID: id, Name: DriveName(id), Volume: v.Name})
		} else {
			p.Volumes = append(p.Volumes, v.Name)
		}
	}
	sort.Strings(p.Volumes)
	sort.Slice(p.Drives, func(i, j int) bool { return p.Drives[i].Name < p.Drives[j].Name })

	dir, err := StateDir(fleetName)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		p.StateDir = dir
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("state dir: %w", err)
		}
		for _, e := range entries {
			if ownStateFile(e.Name()) {
				p.StateFiles = append(p.StateFiles, filepath.Join(dir, e.Name()))
			}
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	return p, nil
}

// DriveName is the name `a12s drive ls` shows for a drive ID: "home-<c>"
// stays, "drive-<name>" becomes "<name>".
func DriveName(id string) string {
	if strings.HasPrefix(id, "home-") {
		return id
	}
	return strings.TrimPrefix(id, "drive-")
}

// computerStopTimeout matches `a12s sleep`: computerd gets a bounded flush.
const computerStopTimeout = docker.StopTimeout

// Down removes the Fleet in two passes. First the Controller, so nothing
// can restart, replace or recreate a Computer. Then it lists the Fleet
// again and removes what is there now: Computers (stopped gracefully, in
// parallel) and any other a12s container of the Fleet, then networks,
// non-drive volumes and the Controller's files in the state directory.
//
// Drives are removed only when purge is true, and only the drives in
// confirmed (the ones the operator was shown); a drive that appeared since
// is kept. Every volume is inspected again right before removal and left
// alone if it no longer belongs to the Fleet or changed kind.
//
// Objects that vanished meanwhile are not errors, so down is idempotent.
// It keeps going after a failure and returns every error.
func Down(ctx context.Context, dc DownDocker, plan *DownPlan, purge bool) (*DownResult, error) {
	res := &DownResult{}
	var errs []error

	for _, c := range plan.Containers {
		if c.Labels[docker.LabelRole] != docker.RoleController {
			continue
		}
		if err := removeContainer(ctx, dc, c, false); err != nil {
			errs = append(errs, err)
		} else {
			res.Containers = append(res.Containers, containerName(c))
		}
	}
	if len(errs) > 0 {
		// A live Controller could recreate what we remove; stop here.
		return res, errors.Join(errs...)
	}

	now, err := PlanDown(ctx, dc, plan.Fleet)
	if err != nil {
		return res, err
	}
	first := len(res.Containers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range now.Containers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := removeContainer(ctx, dc, c, c.Labels[docker.LabelRole] == docker.RoleComputer)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else {
				res.Containers = append(res.Containers, containerName(c))
			}
		}()
	}
	wg.Wait()
	sort.Strings(res.Containers[first:])

	for _, name := range now.Networks {
		if err := removeNetwork(ctx, dc, plan.Fleet, name); err != nil {
			errs = append(errs, err)
		} else {
			res.Networks = append(res.Networks, name)
		}
	}
	for _, name := range now.Volumes {
		if err := removeVolume(ctx, dc, plan.Fleet, name, ""); err != nil {
			errs = append(errs, err)
		} else {
			res.Volumes = append(res.Volumes, name)
		}
	}
	confirmed := map[Drive]bool{}
	for _, d := range plan.Drives {
		confirmed[d] = true
	}
	for _, d := range now.Drives {
		if !purge || !confirmed[d] {
			res.Kept = append(res.Kept, d)
			continue
		}
		if err := removeVolume(ctx, dc, plan.Fleet, d.Volume, d.ID); err != nil {
			errs = append(errs, fmt.Errorf("drive %s: %w", d.Name, err))
			res.Kept = append(res.Kept, d)
		} else {
			res.Destroyed = append(res.Destroyed, d)
		}
	}
	if now.StateDir != "" {
		files, removedDir, err := removeStateFiles(now.StateDir)
		res.StateFiles = files
		if removedDir {
			res.StateDir = now.StateDir
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

func containerName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID
}

// removeContainer stops a Computer gracefully first (computerd flushes its
// journal, the harness gets SIGTERM), then force-removes the container.
// Anonymous volumes go with it; named volumes (drives) never do.
func removeContainer(ctx context.Context, dc DownDocker, c container.Summary, graceful bool) error {
	name := containerName(c)
	if graceful && c.State == container.StateRunning {
		timeout := computerStopTimeout
		if _, err := dc.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}
	if _, err := dc.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil &&
		!cerrdefs.IsNotFound(err) && !cerrdefs.IsConflict(err) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// removeNetwork removes a network after checking it still belongs to the
// Fleet. It never disconnects anything: the Fleet's own containers are gone
// by now, so a remaining endpoint is someone else's, and the network is
// left alone with an error naming it.
func removeNetwork(ctx context.Context, dc DownDocker, fleetName, name string) error {
	n, err := dc.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect network %s: %w", name, err)
	}
	if n.Network.Labels[docker.LabelFleet] != fleetName {
		return fmt.Errorf("network %s is not owned by fleet %s; left alone", name, fleetName)
	}
	if len(n.Network.Containers) > 0 {
		var users []string
		for id, ep := range n.Network.Containers {
			if ep.Name != "" {
				id = ep.Name
			}
			users = append(users, id)
		}
		sort.Strings(users)
		return fmt.Errorf("network %s is still used by %s; left alone (disconnect them and run a12s down again)", name, strings.Join(users, ", "))
	}
	if _, err := dc.NetworkRemove(ctx, n.Network.ID, client.NetworkRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove network %s: %w", name, err)
	}
	return nil
}

// removeVolume removes a volume after inspecting it again: it must still
// carry the Fleet's label and the same drive ID (driveID "" means it must
// still not be a drive). It is never force-removed, so Docker refuses one
// that any container, a12s or not, still mounts.
func removeVolume(ctx context.Context, dc DownDocker, fleetName, name, driveID string) error {
	v, err := dc.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect volume %s: %w", name, err)
	}
	if v.Volume.Labels[docker.LabelFleet] != fleetName || v.Volume.Labels[docker.LabelDrive] != driveID {
		return fmt.Errorf("volume %s changed since it was listed; left alone", name)
	}
	if _, err := dc.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove volume %s: %w", name, err)
	}
	return nil
}

// removeStateFiles removes the files the CLI owns in ~/.a12s/<fleet>
// (controller.json and its temp files), then the directory only if that
// left it empty. Anything else there, such as credentials a bindings file
// points at, is kept. It never follows a symlink.
func removeStateFiles(dir string) (removed []string, removedDir bool, err error) {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("state dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, false, fmt.Errorf("%s is not a directory; left alone", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, fmt.Errorf("state dir: %w", err)
	}
	for _, e := range entries {
		if !ownStateFile(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, false, fmt.Errorf("remove %s: %w", p, err)
		}
		removed = append(removed, p)
	}
	if err := os.Remove(dir); err == nil {
		return removed, true, nil
	} else if errors.Is(err, fs.ErrNotExist) {
		return removed, false, nil
	}
	return removed, false, nil // not empty: other files are kept
}

// ownStateFile reports whether a file in ~/.a12s/<fleet> is the CLI's own.
func ownStateFile(name string) bool {
	return name == controllerFileName || strings.HasPrefix(name, controllerTempPrefix)
}
