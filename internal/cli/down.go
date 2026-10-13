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

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/docker"
)

// Down talks to Docker directly: the Controller cannot remove its own
// container while answering, and down must work when it is gone.

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

type Drive struct {
	ID     string // the a12s.drive label: "home-<computer>" or "state-<service>"
	Name   string // "home-<computer>" or "<service>-state"
	Volume string // the Docker volume
}

type DownPlan struct {
	Fleet      string
	Containers []container.Summary // Controller first, then Computers, then anything else
	Networks   []string
	Volumes    []string // non-drive volumes: Controller state, bundle cache
	Drives     []Drive
	// Data volumes hold fleet.db with the inboxes: kept like drives, destroyed only by purge.
	Data       []string
	StateDir   string   // ~/.a12s/<fleet>, "" when absent or not a directory
	StateFiles []string // the CLI's own files in it (controller.json)
}

func (p *DownPlan) Empty() bool {
	return len(p.Containers) == 0 && len(p.Networks) == 0 && len(p.Volumes) == 0 && len(p.StateFiles) == 0
}

type DownResult struct {
	Containers    []string // names
	Networks      []string
	Volumes       []string
	StateFiles    []string // removed from the state dir
	StateDir      string   // removed (it was empty after StateFiles), or ""
	Kept          []Drive  // drives left in place
	Destroyed     []Drive  // drives removed (purge only)
	DataKept      []string // data volumes left in place
	DataDestroyed []string // data volumes removed (purge only)
}

func fleetFilter(fleetName string) client.Filters {
	return make(client.Filters).Add("label", docker.LabelFleet+"="+fleetName)
}

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
		if _, isData := v.Labels[docker.LabelData]; isData {
			p.Data = append(p.Data, v.Name)
			continue
		}
		if id, isDrive := v.Labels[docker.LabelDrive]; isDrive {
			// A drive label without a value is still a drive: kept, never purged.
			name := DriveName(id)
			if id == "" {
				name = v.Name
			}
			p.Drives = append(p.Drives, Drive{ID: id, Name: name, Volume: v.Name})
		} else {
			p.Volumes = append(p.Volumes, v.Name)
		}
	}
	sort.Strings(p.Volumes)
	sort.Strings(p.Data)
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

func DriveName(id string) string {
	if svc, ok := driver.ServiceOfStateDrive(id); ok {
		return driver.StateDriveName(svc)
	}
	return id
}

const computerStopTimeout = docker.StopTimeout

// Down removes every Controller first so nothing can recreate a Computer, and
// purges only the drives the operator confirmed. Vanished objects are not errors.
func Down(ctx context.Context, dc DownDocker, plan *DownPlan, purge bool) (*DownResult, error) {
	res := &DownResult{}
	var errs []error

	// List again until no Controller remains: one may have started while the purge
	// prompt was open. The plan is only trusted for which drives were confirmed.
	var now *DownPlan
	for round := 0; ; round++ {
		var err error
		if now, err = PlanDown(ctx, dc, plan.Fleet); err != nil {
			return res, err
		}
		ctrl := controllers(now.Containers)
		if len(ctrl) == 0 {
			break
		}
		if round == maxControllerRounds {
			return res, fmt.Errorf("a Controller of fleet %s keeps reappearing (%s); stop whatever starts it and run a12s down again", plan.Fleet, containerName(ctrl[0]))
		}
		for _, c := range ctrl {
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
	}
	first := len(res.Containers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range now.Containers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			role := c.Labels[docker.LabelRole]
			err := removeContainer(ctx, dc, c, role == docker.RoleComputer || role == docker.RoleService)
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
		if !purge || !confirmed[d] || d.ID == "" {
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
	confirmedData := map[string]bool{}
	for _, v := range plan.Data {
		confirmedData[v] = true
	}
	for _, v := range now.Data {
		if !purge || !confirmedData[v] {
			res.DataKept = append(res.DataKept, v)
			continue
		}
		if err := removeDataVolume(ctx, dc, plan.Fleet, v); err != nil {
			errs = append(errs, fmt.Errorf("data volume %s: %w", v, err))
			res.DataKept = append(res.DataKept, v)
		} else {
			res.DataDestroyed = append(res.DataDestroyed, v)
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

const maxControllerRounds = 3

func controllers(cs []container.Summary) []container.Summary {
	var out []container.Summary
	for _, c := range cs {
		if c.Labels[docker.LabelRole] == docker.RoleController {
			out = append(out, c)
		}
	}
	return out
}

func containerName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID
}

// removeContainer stops gracefully first so computerd flushes its journal.
// Named volumes (drives) never go with the container.
func removeContainer(ctx context.Context, dc DownDocker, c container.Summary, graceful bool) error {
	name := containerName(c)
	if graceful && c.State == container.StateRunning {
		timeout := computerStopTimeout
		if _, err := dc.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}
	// Conflict (a removal already in progress) is an error, not success:
	// down must know the container is gone before it goes on.
	if _, err := dc.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// removeNetwork never disconnects anything: the Fleet's containers are gone by
// now, so a remaining endpoint is someone else's.
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

// removeVolume never force-removes, so Docker refuses a volume any container still
// mounts. driveID "" means it must carry no drive label at all.
func removeVolume(ctx context.Context, dc DownDocker, fleetName, name, driveID string) error {
	v, err := dc.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect volume %s: %w", name, err)
	}
	id, isDrive := v.Volume.Labels[docker.LabelDrive]
	if _, isData := v.Volume.Labels[docker.LabelData]; isData {
		return fmt.Errorf("volume %s is a data volume; left alone", name)
	}
	if v.Volume.Labels[docker.LabelFleet] != fleetName || isDrive != (driveID != "") || id != driveID {
		return fmt.Errorf("volume %s changed since it was listed; left alone", name)
	}
	if _, err := dc.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove volume %s: %w", name, err)
	}
	return nil
}

func removeDataVolume(ctx context.Context, dc DownDocker, fleetName, name string) error {
	v, err := dc.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect volume %s: %w", name, err)
	}
	_, isData := v.Volume.Labels[docker.LabelData]
	_, isDrive := v.Volume.Labels[docker.LabelDrive]
	if v.Volume.Labels[docker.LabelFleet] != fleetName || !isData || isDrive {
		return fmt.Errorf("volume %s changed since it was listed; left alone", name)
	}
	if _, err := dc.VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove volume %s: %w", name, err)
	}
	return nil
}

// removeStateFiles keeps anything it does not own, such as credential files an older
// a12s wrote, and never follows a symlink.
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

func ownStateFile(name string) bool {
	return name == controllerFileName || strings.HasPrefix(name, controllerTempPrefix)
}
