package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// fakeDocker holds containers, networks and volumes by name and records
// every mutating call. Its List calls ignore filters, so PlanDown's own
// label check is what keeps other fleets out.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]container.Summary
	networks   map[string]network.Inspect
	volumes    map[string]volume.Volume
	calls      []string
	filters    []client.Filters
	// stopFail fails ContainerStop for this container name.
	stopFail string
	// relabel changes a network's fleet label at inspect time.
	relabel map[string]string
	// conflict makes ContainerRemove of these names fail with Conflict.
	conflict map[string]bool
	// onRemove runs (under the lock) after a container is removed.
	onRemove map[string]func()
}

func labels(fleetName, role string, extra ...string) map[string]string {
	l := map[string]string{"a12s.fleet": fleetName}
	if role != "" {
		l["a12s.role"] = role
	}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return l
}

func newFake() *fakeDocker {
	f := &fakeDocker{
		containers: map[string]container.Summary{},
		networks:   map[string]network.Inspect{},
		volumes:    map[string]volume.Volume{},
	}
	addC := func(name string, l map[string]string, st container.ContainerState) {
		f.containers[name] = container.Summary{ID: "id-" + name, Names: []string{"/" + name}, Labels: l, State: st}
	}
	addC("a12s-qs-controller", labels("qs", "controller"), container.StateRunning)
	addC("a12s-qs-hello", labels("qs", "computer", "a12s.computer", "hello"), container.StateRunning)
	addC("a12s-qs-idle", labels("qs", "computer", "a12s.computer", "idle"), container.StateExited)
	addC("a12s-qs-extract", labels("qs", ""), container.StateCreated)
	addC("a12s-other-controller", labels("other", "controller"), container.StateRunning)
	addC("a12s-other-hello", labels("other", "computer"), container.StateRunning)

	addN := func(name string, l map[string]string, members ...string) {
		eps := map[string]network.EndpointResource{}
		for _, m := range members {
			eps["id-"+m] = network.EndpointResource{Name: m}
		}
		f.networks[name] = network.Inspect{
			Network:    network.Network{Name: name, ID: "net-" + name, Labels: l},
			Containers: eps,
		}
	}
	addN("a12s-qs", labels("qs", "controller"), "a12s-qs-controller")
	addN("a12s-qs-hello", labels("qs", "computer"), "a12s-qs-controller", "a12s-qs-hello")
	addN("a12s-other", labels("other", "controller"), "a12s-other-controller")

	addV := func(name string, l map[string]string) { f.volumes[name] = volume.Volume{Name: name, Labels: l} }
	addV("a12s-qs-controller", labels("qs", "controller"))
	addV("a12s-qs-bundles", labels("qs", ""))
	addV("a12s-qs-home-hello", labels("qs", "", "a12s.drive", "home-hello", "a12s.computer", "hello"))
	addV("a12s-qs-drive-notes", labels("qs", "", "a12s.drive", "drive-notes"))
	addV("a12s-other-home-hello", labels("other", "", "a12s.drive", "home-hello"))
	addV("a12s-other-controller", labels("other", "controller"))
	addV("unrelated", nil)
	return f
}

func (f *fakeDocker) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeDocker) byID(id string) (string, bool) {
	for name, c := range f.containers {
		if c.ID == id {
			return name, true
		}
	}
	return "", false
}

func (f *fakeDocker) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, o.Filters)
	var r client.ContainerListResult
	for _, c := range f.containers {
		r.Items = append(r.Items, c)
	}
	return r, nil
}

func (f *fakeDocker) ContainerStop(_ context.Context, id string, o client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	name, ok := f.byID(id)
	f.mu.Unlock()
	if !ok {
		return client.ContainerStopResult{}, cerrdefs.ErrNotFound
	}
	if o.Timeout == nil || *o.Timeout != computerStopTimeout {
		return client.ContainerStopResult{}, errors.New("stop without the sleep timeout")
	}
	if name == f.stopFail {
		return client.ContainerStopResult{}, errors.New("daemon hiccup")
	}
	f.record("stop " + name)
	return client.ContainerStopResult{}, nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.byID(id)
	if !ok {
		return client.ContainerRemoveResult{}, cerrdefs.ErrNotFound
	}
	if f.conflict[name] {
		return client.ContainerRemoveResult{}, cerrdefs.ErrConflict
	}
	if !o.Force || o.RemoveVolumes {
		return client.ContainerRemoveResult{}, errors.New("unexpected remove options")
	}
	delete(f.containers, name)
	for _, n := range f.networks {
		delete(n.Containers, id)
	}
	f.calls = append(f.calls, "rm "+name)
	if hook := f.onRemove[name]; hook != nil {
		hook()
	}
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeDocker) NetworkList(_ context.Context, o client.NetworkListOptions) (client.NetworkListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, o.Filters)
	var r client.NetworkListResult
	for _, n := range f.networks {
		r.Items = append(r.Items, network.Summary{Network: n.Network})
	}
	return r, nil
}

func (f *fakeDocker) NetworkInspect(_ context.Context, id string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[id]
	if !ok {
		return client.NetworkInspectResult{}, cerrdefs.ErrNotFound
	}
	if fl, ok := f.relabel[id]; ok {
		n.Labels = labels(fl, "")
	}
	return client.NetworkInspectResult{Network: n}, nil
}

func (f *fakeDocker) NetworkDisconnect(context.Context, string, client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error) {
	return client.NetworkDisconnectResult{}, errors.New("down must never disconnect anything")
}

func (f *fakeDocker) NetworkRemove(_ context.Context, netID string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strings.TrimPrefix(netID, "net-") // removal is by ID
	if _, ok := f.networks[id]; !ok || netID == id {
		return client.NetworkRemoveResult{}, cerrdefs.ErrNotFound
	}
	if len(f.networks[id].Containers) > 0 {
		return client.NetworkRemoveResult{}, errors.New("network has active endpoints")
	}
	delete(f.networks, id)
	f.calls = append(f.calls, "rmnet "+id)
	return client.NetworkRemoveResult{}, nil
}

func (f *fakeDocker) VolumeList(_ context.Context, o client.VolumeListOptions) (client.VolumeListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, o.Filters)
	var r client.VolumeListResult
	for _, v := range f.volumes {
		r.Items = append(r.Items, v)
	}
	return r, nil
}

func (f *fakeDocker) VolumeInspect(_ context.Context, id string, _ client.VolumeInspectOptions) (client.VolumeInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[id]
	if !ok {
		return client.VolumeInspectResult{}, cerrdefs.ErrNotFound
	}
	return client.VolumeInspectResult{Volume: v}, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, id string, o client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o.Force {
		return client.VolumeRemoveResult{}, errors.New("volumes are never force-removed")
	}
	if _, ok := f.volumes[id]; !ok {
		return client.VolumeRemoveResult{}, cerrdefs.ErrNotFound
	}
	delete(f.volumes, id)
	f.calls = append(f.calls, "rmvol "+id)
	return client.VolumeRemoveResult{}, nil
}

func (f *fakeDocker) names() (cs, ns, vs []string) {
	for n := range f.containers {
		cs = append(cs, n)
	}
	for n := range f.networks {
		ns = append(ns, n)
	}
	for n := range f.volumes {
		vs = append(vs, n)
	}
	slices.Sort(cs)
	slices.Sort(ns)
	slices.Sort(vs)
	return
}

// withHome points ~ at a temp dir holding ~/.a12s/qs/controller.json and a
// credential file ~/.a12s/anthropic.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".a12s", "qs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for p, data := range map[string]string{".a12s/qs/controller.json": "{}", ".a12s/anthropic": "sk-ant-oat-x"} {
		if err := os.WriteFile(filepath.Join(home, p), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestDownKeepsDrives(t *testing.T) {
	home := withHome(t)
	ctx := context.Background()
	f := newFake()
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	for _, flt := range f.filters {
		if !flt["label"]["a12s.fleet=qs"] {
			t.Errorf("list without the fleet label filter: %v", flt)
		}
	}
	wantDrives := []Drive{{ID: "home-hello", Name: "home-hello", Volume: "a12s-qs-home-hello"}, {ID: "drive-notes", Name: "notes", Volume: "a12s-qs-drive-notes"}}
	if !slices.Equal(plan.Drives, wantDrives) {
		t.Errorf("drives %+v", plan.Drives)
	}
	res, err := Down(ctx, f, plan, false)
	if err != nil {
		t.Fatal(err)
	}

	// The Controller goes first, so it cannot restart or recreate anything;
	// only the running Computer is stopped (gracefully) before removal.
	if f.calls[0] != "rm a12s-qs-controller" {
		t.Errorf("first call %q, want the controller removed", f.calls[0])
	}
	if i, j := slices.Index(f.calls, "stop a12s-qs-hello"), slices.Index(f.calls, "rm a12s-qs-hello"); i < 0 || j < i {
		t.Errorf("hello not stopped before removal: %v", f.calls)
	}
	for _, c := range f.calls {
		if c == "stop a12s-qs-idle" || c == "stop a12s-qs-extract" {
			t.Errorf("unexpected %q", c)
		}
	}
	cs, ns, vs := f.names()
	if !slices.Equal(cs, []string{"a12s-other-controller", "a12s-other-hello"}) {
		t.Errorf("containers left %v", cs)
	}
	if !slices.Equal(ns, []string{"a12s-other"}) {
		t.Errorf("networks left %v", ns)
	}
	if !slices.Equal(vs, []string{"a12s-other-controller", "a12s-other-home-hello", "a12s-qs-drive-notes", "a12s-qs-home-hello", "unrelated"}) {
		t.Errorf("volumes left %v", vs)
	}
	if !slices.Equal(res.Kept, wantDrives) || len(res.Destroyed) != 0 {
		t.Errorf("kept %+v destroyed %+v", res.Kept, res.Destroyed)
	}
	if !slices.Equal(res.Containers, []string{"a12s-qs-controller", "a12s-qs-extract", "a12s-qs-hello", "a12s-qs-idle"}) {
		t.Errorf("result containers %v", res.Containers)
	}
	if _, err := os.Stat(filepath.Join(home, ".a12s", "qs")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state dir still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".a12s", "anthropic")); err != nil {
		t.Errorf("credential file touched: %v", err)
	}

	// Twice: nothing left but the kept drives, and that is success.
	plan, err = PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() || len(plan.Drives) != 2 {
		t.Errorf("second plan %+v", plan)
	}
	n := len(f.calls)
	if res, err = Down(ctx, f, plan, false); err != nil || len(f.calls) != n || len(res.Kept) != 2 {
		t.Errorf("second down: err %v calls %v kept %v", err, f.calls[n:], res.Kept)
	}
}

func TestDownPurge(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Down(ctx, f, plan, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, vs := f.names()
	if !slices.Equal(vs, []string{"a12s-other-controller", "a12s-other-home-hello", "unrelated"}) {
		t.Errorf("volumes left %v", vs)
	}
	if len(res.Kept) != 0 || len(res.Destroyed) != 2 {
		t.Errorf("kept %+v destroyed %+v", res.Kept, res.Destroyed)
	}
	// Drives go only after every container that could mount them.
	last := slices.Index(f.calls, "rm a12s-qs-hello")
	for i, c := range f.calls {
		if strings.HasPrefix(c, "rmvol a12s-qs-home") && i < last {
			t.Errorf("drive removed before its Computer: %v", f.calls)
		}
	}
}

// Down when the Plan is stale: everything vanished meanwhile (another down
// ran, or Docker was reset). NotFound is success.
func TestDownStalePlanIsIdempotent(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Down(ctx, f, plan, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Down(ctx, f, plan, true); err != nil {
		t.Errorf("stale replay: %v", err)
	}
}

func TestDownNoController(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	delete(f.containers, "a12s-qs-controller")
	for _, n := range f.networks {
		delete(n.Containers, "id-a12s-qs-controller")
	}
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Down(ctx, f, plan, false); err != nil {
		t.Fatal(err)
	}
	if cs, _, _ := f.names(); len(cs) != 2 {
		t.Errorf("containers left %v", cs)
	}
}

func TestDownNeverTouchesAnotherFleet(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	// The network was re-labeled after the plan was made.
	f.relabel = map[string]string{"a12s-qs-hello": "other"}
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Down(ctx, f, plan, false)
	if err == nil || !strings.Contains(err.Error(), "not owned by fleet qs") {
		t.Fatalf("err %v", err)
	}
	if _, ns, _ := f.names(); !slices.Contains(ns, "a12s-qs-hello") {
		t.Errorf("relabeled network removed")
	}
	for _, c := range f.calls {
		if strings.Contains(c, "other") {
			t.Errorf("touched another fleet: %q", c)
		}
	}
}

func TestDownKeepsGoingAfterAnError(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	f.stopFail = "a12s-qs-hello"
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Down(ctx, f, plan, false)
	if err == nil || !strings.Contains(err.Error(), "stop a12s-qs-hello") {
		t.Fatalf("err %v", err)
	}
	if slices.Contains(res.Containers, "a12s-qs-hello") || !slices.Contains(res.Containers, "a12s-qs-idle") {
		t.Errorf("containers %v", res.Containers)
	}
	if res.StateDir == "" {
		t.Errorf("state dir not removed after a container error")
	}
}

// A fleet whose state-dir path is not a directory (here, a fleet named like
// the credential file) leaves the file alone.
func TestDownStateDirMustBeADirectory(t *testing.T) {
	home := withHome(t)
	ctx := context.Background()
	plan, err := PlanDown(ctx, newFake(), "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if plan.StateDir != "" || len(plan.StateFiles) != 0 {
		t.Errorf("state dir %q files %v", plan.StateDir, plan.StateFiles)
	}
	if _, _, err := removeStateFiles(filepath.Join(home, ".a12s", "anthropic")); err == nil {
		t.Error("treated a file as the state dir")
	}
	if _, err := os.Stat(filepath.Join(home, ".a12s", "anthropic")); err != nil {
		t.Errorf("credential file gone: %v", err)
	}
}

// ~/.a12s/lab holds the credentials examples/lab binds: down removes only
// controller.json and keeps the directory with the rest.
func TestDownKeepsOtherFilesInStateDir(t *testing.T) {
	home := withHome(t)
	dir := filepath.Join(home, ".a12s", "qs")
	for _, n := range []string{"anthropic.json", ".controller.json-123"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	f := newFake()
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.StateFiles) != 2 {
		t.Errorf("state files %v", plan.StateFiles)
	}
	res, err := Down(ctx, f, plan, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.StateDir != "" || len(res.StateFiles) != 2 {
		t.Errorf("state dir %q files %v", res.StateDir, res.StateFiles)
	}
	left, _ := os.ReadDir(dir)
	if len(left) != 1 || left[0].Name() != "anthropic.json" {
		t.Errorf("left %v", left)
	}
	if plan, _ := PlanDown(ctx, f, "qs"); !plan.Empty() {
		t.Errorf("not empty after down: %+v", plan)
	}
}

// The Controller replaced a Computer after the plan was made (a staged
// generation). Down lists again once the Controller is gone, so the
// replacement is removed too.
func TestDownRelistsAfterTheController(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	f.onRemove = map[string]func(){"a12s-qs-controller": func() {
		c := f.containers["a12s-qs-hello"]
		c.ID = "id-hello-gen2"
		f.containers["a12s-qs-hello"] = c
		delete(f.networks["a12s-qs-hello"].Containers, "id-a12s-qs-hello")
		f.networks["a12s-qs-hello"].Containers["id-hello-gen2"] = network.EndpointResource{Name: "a12s-qs-hello"}
	}}
	if _, err := Down(ctx, f, plan, false); err != nil {
		t.Fatal(err)
	}
	cs, ns, _ := f.names()
	if slices.Contains(cs, "a12s-qs-hello") || slices.Contains(ns, "a12s-qs-hello") {
		t.Errorf("replacement left behind: %v %v", cs, ns)
	}
}

// A container of another fleet (or not a12s at all) attached to the
// Fleet's network is never disconnected; the network is left alone.
func TestDownLeavesForeignEndpoints(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	f.networks["a12s-qs"].Containers["id-a12s-other-hello"] = network.EndpointResource{Name: "a12s-other-hello"}
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Down(ctx, f, plan, false)
	if err == nil || !strings.Contains(err.Error(), "network a12s-qs is still used by a12s-other-hello") {
		t.Fatalf("err %v", err)
	}
	if _, ns, _ := f.names(); !slices.Contains(ns, "a12s-qs") || slices.Contains(ns, "a12s-qs-hello") {
		t.Errorf("networks %v", ns)
	}
}

// Volumes are inspected again before removal: one recreated under the same
// name by someone else, or a drive where the plan saw a cache volume, stays.
// A drive that appeared after the purge prompt is not destroyed.
func TestDownRevalidatesVolumes(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	f.volumes["a12s-qs-home-hello"] = volume.Volume{Name: "a12s-qs-home-hello", Labels: labels("other", "")}
	f.volumes["a12s-qs-late"] = volume.Volume{Name: "a12s-qs-late", Labels: labels("qs", "", "a12s.drive", "drive-late")}
	res, err := Down(ctx, f, plan, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, vs := f.names()
	for _, want := range []string{"a12s-qs-home-hello", "a12s-qs-late"} {
		if !slices.Contains(vs, want) {
			t.Errorf("%s removed; left %v", want, vs)
		}
	}
	if len(res.Destroyed) != 1 || res.Destroyed[0].Name != "notes" {
		t.Errorf("destroyed %+v", res.Destroyed)
	}
	if !slices.Contains(res.Kept, Drive{ID: "drive-late", Name: "late", Volume: "a12s-qs-late"}) {
		t.Errorf("kept %+v", res.Kept)
	}

	// Kind changed between listing and removal.
	f2 := newFake()
	if err := removeVolume(ctx, f2, "qs", "a12s-qs-home-hello", ""); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Errorf("drive removed as a cache volume: %v", err)
	}
	if err := removeVolume(ctx, f2, "qs", "a12s-other-controller", ""); err == nil {
		t.Error("removed another fleet's volume")
	}
}

// A volume whose drive label is present but empty is still a drive: plain
// down keeps it, purge keeps it too, and it is never treated as a cache.
func TestDownEmptyDriveLabelIsKept(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	f.volumes["a12s-qs-odd"] = volume.Volume{Name: "a12s-qs-odd", Labels: labels("qs", "", "a12s.drive", "")}
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plan.Volumes, "a12s-qs-odd") {
		t.Fatal("empty drive label classified as a cache volume")
	}
	res, err := Down(ctx, f, plan, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, vs := f.names(); !slices.Contains(vs, "a12s-qs-odd") {
		t.Error("purged a drive with an empty label")
	}
	if !slices.Contains(res.Kept, Drive{Name: "a12s-qs-odd", Volume: "a12s-qs-odd"}) {
		t.Errorf("kept %+v", res.Kept)
	}
	if err := removeVolume(ctx, f, "qs", "a12s-qs-odd", ""); err == nil {
		t.Error("removed an empty-label drive as a non-drive volume")
	}
}

// Conflict (a removal already in progress) is not success: with the
// Controller, down stops before touching anything else.
func TestDownControllerConflictStops(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	f.conflict = map[string]bool{"a12s-qs-controller": true}
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Down(ctx, f, plan, true); err == nil || !strings.Contains(err.Error(), "remove a12s-qs-controller") {
		t.Fatalf("err %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("went on after the Controller conflict: %v", f.calls)
	}
}

// A Controller that appeared after the plan (e.g. while the purge prompt
// was open) is removed before any Computer, and its drive is not purged.
func TestDownControllerStartedAfterThePlan(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	ctrl := f.containers["a12s-qs-controller"]
	delete(f.containers, "a12s-qs-controller")
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	ctrl.ID = "id-ctrl-new"
	f.containers["a12s-qs-controller"] = ctrl
	f.volumes["a12s-qs-new"] = volume.Volume{Name: "a12s-qs-new", Labels: labels("qs", "", "a12s.drive", "drive-new")}
	for _, n := range f.networks {
		delete(n.Containers, "id-a12s-qs-controller")
	}
	if _, err := Down(ctx, f, plan, true); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "rm a12s-qs-controller" {
		t.Errorf("calls %v", f.calls)
	}
	if _, _, vs := f.names(); !slices.Contains(vs, "a12s-qs-new") {
		t.Error("purged a drive the operator was not shown")
	}
}

func TestDownControllerKeepsReappearing(t *testing.T) {
	withHome(t)
	ctx := context.Background()
	f := newFake()
	ctrl := f.containers["a12s-qs-controller"]
	f.onRemove = map[string]func(){"a12s-qs-controller": func() { f.containers["a12s-qs-controller"] = ctrl }}
	plan, err := PlanDown(ctx, f, "qs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Down(ctx, f, plan, false); err == nil || !strings.Contains(err.Error(), "keeps reappearing") {
		t.Fatalf("err %v", err)
	}
	if cs, _, _ := f.names(); !slices.Contains(cs, "a12s-qs-hello") {
		t.Error("removed Computers while a Controller was still coming back")
	}
}

func TestDriveName(t *testing.T) {
	for id, want := range map[string]string{"home-hello": "home-hello", "drive-notes": "notes", "drive-home-x": "home-x"} {
		if got := DriveName(id); got != want {
			t.Errorf("DriveName(%q) = %q, want %q", id, got, want)
		}
	}
}
