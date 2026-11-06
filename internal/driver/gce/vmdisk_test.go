package gce

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/client"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/driver/engine"
)

// fakeEngine is just enough of a Docker engine for Materialize: images with sizes on a disk with a fixed capacity,
// containers, volumes. A load stages the whole archive before it registers it, as the engine does.
type fakeEngine struct {
	mu         sync.Mutex
	capacity   int64
	images     map[string]*fakeImage // by ID
	containers map[string]*fakeContainer
	volumes    map[string]map[string]string
	loads      []string // IDs loaded, in order
	nextID     int
}

type fakeImage struct {
	size int64
	tags []string
}

type fakeContainer struct {
	id, name, image string
	labels          map[string]string
	entrypoint      []string
	exited          bool
}

func newFakeEngine(capacity int64) *fakeEngine {
	return &fakeEngine{capacity: capacity, images: map[string]*fakeImage{}, containers: map[string]*fakeContainer{}, volumes: map[string]map[string]string{}}
}

func (f *fakeEngine) usedLocked() int64 {
	var n int64
	for _, im := range f.images {
		n += im.size
	}
	return n
}

func (f *fakeEngine) imageLocked(ref string) (string, *fakeImage) {
	if im, ok := f.images[ref]; ok {
		return ref, im
	}
	for id, im := range f.images {
		if slices.Contains(im.tags, ref) {
			return id, im
		}
	}
	return "", nil
}

func (f *fakeEngine) containerLocked(ref string) *fakeContainer {
	for _, c := range f.containers {
		if c.id == ref || c.name == ref {
			return c
		}
	}
	return nil
}

func (f *fakeEngine) imageIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.images {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

var versionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter, what string) {
	w.WriteHeader(http.StatusNotFound)
	writeJSON(w, map[string]string{"message": "No such " + what})
}

func pathStat(w http.ResponseWriter, name string) {
	b, _ := json.Marshal(map[string]any{"name": name, "size": 1, "mode": 0o644})
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(b))
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := versionPrefix.ReplaceAllString(r.URL.Path, "")
	w.Header().Set("Api-Version", "1.44")
	switch {
	case p == "/_ping":
		w.Write([]byte("OK"))

	case r.Method == http.MethodGet && p == "/images/json":
		var out []map[string]any
		for id, im := range f.images {
			out = append(out, map[string]any{"Id": id, "Size": im.size, "RepoTags": im.tags, "Containers": -1})
		}
		writeJSON(w, out)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
		id, im := f.imageLocked(strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json"))
		if im == nil {
			notFound(w, "image")
			return
		}
		writeJSON(w, map[string]any{"Id": id, "Size": im.size, "RepoTags": im.tags})
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/images/"):
		id, im := f.imageLocked(strings.TrimPrefix(p, "/images/"))
		if im == nil {
			notFound(w, "image")
			return
		}
		for _, c := range f.containers {
			if c.image == id && r.URL.Query().Get("force") != "1" {
				w.WriteHeader(http.StatusConflict)
				writeJSON(w, map[string]string{"message": "image is being used"})
				return
			}
		}
		delete(f.images, id)
		writeJSON(w, []map[string]string{{"Deleted": id}})
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/tag"):
		_, im := f.imageLocked(strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/tag"))
		if im == nil {
			notFound(w, "image")
			return
		}
		im.tags = append(im.tags, r.URL.Query().Get("repo")+":"+r.URL.Query().Get("tag"))
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && p == "/images/get":
		// The Controller's engine: the archive names the image and its size.
		id, im := f.imageLocked(r.URL.Query().Get("names"))
		if im == nil {
			notFound(w, "image")
			return
		}
		fmt.Fprintf(w, "%s %d", id, im.size)
	case r.Method == http.MethodPost && p == "/images/load":
		var src string
		var size int64
		body, _ := io.ReadAll(r.Body)
		fmt.Sscanf(string(body), "%s %d", &src, &size)
		id := "sha256:vm-" + strings.TrimPrefix(src, "sha256:")
		f.loads = append(f.loads, src)
		if free := f.capacity - f.usedLocked(); free < 2*size {
			writeJSON(w, map[string]string{"error": "write /var/lib/docker/tmp/docker-import-1/blob: no space left on device"})
			return
		}
		f.images[id] = &fakeImage{size: size}
		writeJSON(w, map[string]string{"stream": "Loaded image ID: " + id + "\n"})

	case r.Method == http.MethodGet && p == "/containers/json":
		var out []map[string]any
		for _, c := range f.containers {
			out = append(out, map[string]any{"Id": c.id, "Names": []string{"/" + c.name}, "ImageID": c.image, "Image": c.image})
		}
		writeJSON(w, out)
	case r.Method == http.MethodPost && p == "/containers/create":
		var body struct {
			Image      string
			Entrypoint []string
			Labels     map[string]string
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := r.URL.Query().Get("name")
		if f.containerLocked(name) != nil {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]string{"message": "Conflict. The container name is already in use"})
			return
		}
		id, im := f.imageLocked(body.Image)
		if im == nil {
			notFound(w, "image")
			return
		}
		f.nextID++
		c := &fakeContainer{id: fmt.Sprintf("c%d", f.nextID), name: name, image: id, labels: body.Labels, entrypoint: body.Entrypoint}
		f.containers[c.id] = c
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"Id": c.id})
	case strings.HasPrefix(p, "/containers/"):
		rest := strings.TrimPrefix(p, "/containers/")
		ref, op, _ := strings.Cut(rest, "/")
		c := f.containerLocked(ref)
		if c == nil {
			notFound(w, "container")
			return
		}
		switch {
		case r.Method == http.MethodGet && op == "json":
			writeJSON(w, map[string]any{"Id": c.id, "Name": "/" + c.name, "Image": c.image,
				"State": map[string]any{"Running": false, "Status": "exited"}, "Config": map[string]any{"Labels": c.labels}})
		case r.Method == http.MethodDelete && op == "":
			delete(f.containers, c.id)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && op == "start":
			c.exited = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && op == "wait":
			writeJSON(w, map[string]any{"StatusCode": 0})
		case r.Method == http.MethodGet && op == "logs":
			if !slices.Equal(c.entrypoint, []string{"df", "-P", "-k", "/"}) {
				w.WriteHeader(http.StatusOK)
				return
			}
			free := (f.capacity - f.usedLocked()) / 1024
			out := fmt.Sprintf("Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay %d %d %d 50%% /\n",
				f.capacity/1024, f.usedLocked()/1024, free)
			// One stdout frame of the engine's multiplexed log stream.
			hdr := []byte{1, 0, 0, 0, 0, 0, 0, 0}
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(out)))
			w.Write(append(hdr, out...))
		case r.Method == http.MethodHead && op == "archive":
			pathStat(w, "bundle.json") // every bundle is already in the cache
		case r.Method == http.MethodGet && op == "archive":
			pathStat(w, "bundle.json")
			tw := tar.NewWriter(w)
			m := []byte(`{"kind":"codex","version":"1.0.0","bin":"bin/codex"}`)
			tw.WriteHeader(&tar.Header{Name: "bundle.json", Mode: 0o644, Size: int64(len(m))})
			tw.Write(m)
			tw.Close()
		default:
			http.Error(w, "fake engine: "+r.Method+" "+p, http.StatusNotImplemented)
		}

	case r.Method == http.MethodGet && strings.HasPrefix(p, "/volumes/"):
		labels, ok := f.volumes[strings.TrimPrefix(p, "/volumes/")]
		if !ok {
			notFound(w, "volume")
			return
		}
		writeJSON(w, map[string]any{"Name": strings.TrimPrefix(p, "/volumes/"), "Labels": labels})
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/volumes/"):
		delete(f.volumes, strings.TrimPrefix(p, "/volumes/"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && p == "/volumes/create":
		var body struct {
			Name   string
			Labels map[string]string
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.volumes[body.Name] = body.Labels
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"Name": body.Name, "Labels": body.Labels})
	default:
		http.Error(w, "fake engine: "+r.Method+" "+p, http.StatusNotImplemented)
	}
}

func fakeClient(t *testing.T, f *fakeEngine) *client.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithAPIVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

const gb1 = int64(1) << 30

// silicoVM is an agent VM like the 2026-10-27 Silico ones: a disk with room for about two toolbox images, running
// generation 2 on toolbox:2 with toolbox:1 still left from generation 1, and the Controller holding toolbox:3.
func silicoVM(t *testing.T, capacity int64) (*vmGuest, *fakeEngine, driver.ComputerSpec) {
	t.Helper()
	ctrl := newFakeEngine(100 * gb1)
	ctrl.images["sha256:tb3"] = &fakeImage{size: 2 * gb1, tags: []string{"silico-toolbox:3"}}
	ctrl.images["sha256:bundle"] = &fakeImage{size: gb1 / 4, tags: []string{"a12s/bundle-codex:1.0.0"}}
	vm := newFakeEngine(capacity)
	vm.images["sha256:vm-tb1"] = &fakeImage{size: 2 * gb1, tags: []string{"a12s.local/image:tb1"}}
	vm.images["sha256:vm-tb2"] = &fakeImage{size: 2 * gb1, tags: []string{"a12s.local/image:tb2"}}
	vm.images["sha256:vm-bundle"] = &fakeImage{size: gb1 / 4, tags: []string{"a12s.local/image:bundle"}}
	vm.containers["old"] = &fakeContainer{id: "old", name: "a12s-silico-helix", image: "sha256:vm-tb2",
		labels: map[string]string{engine.LabelGeneration: "2"}}
	vm.volumes[BundleVolume("silico")] = map[string]string{engine.LabelFleet: "silico", labelBundle: "sha256:bundle"}
	g := &vmGuest{
		e:   &Engines{Fleet: "silico", Local: engine.New(fakeClient(t, ctrl), "silico")},
		vm:  VM{Name: "a12s-silico-silico-helix", Computer: "helix"},
		eng: engine.New(fakeClient(t, vm), "silico"),
	}
	spec := driver.ComputerSpec{
		Name: "helix", Generation: 3, Image: "silico-toolbox:3", Tier: "slim",
		Bundle:    driver.BundleRef{Kind: "codex", Version: "1.0.0", Image: "sha256:bundle"},
		HomeDrive: driver.HomeDriveID("helix"),
	}
	return g, vm, spec
}

// Switching a VM to a new image leaves only the images its Computer runs: the old generation's image and any older
// one go, so a disk that held two images takes the next one.
func TestMaterializeLeavesOnlyTheImagesItRuns(t *testing.T) {
	g, vm, spec := silicoVM(t, 7*gb1) // 6.25 GB used: toolbox:3 doesn't fit unless the old images go
	if err := g.Materialize(context.Background(), spec); err != nil {
		t.Fatalf("Materialize = %v", err)
	}
	if got, want := vm.imageIDs(), []string{"sha256:vm-bundle", "sha256:vm-tb3"}; !slices.Equal(got, want) {
		t.Fatalf("images on the VM = %v, want %v", got, want)
	}
	if c := vm.containerLocked("a12s-silico-helix"); c == nil || c.image != "sha256:vm-tb3" || c.labels[engine.LabelGeneration] != "3" {
		t.Fatalf("container = %+v", c)
	}
}

// When even an empty disk can't take the image, Materialize says so before loading anything, with the bytes free
// and needed, instead of failing half-way through a load.
func TestMaterializeFailsClearlyWhenTheImageCantFit(t *testing.T) {
	g, vm, spec := silicoVM(t, 4*gb1) // empty but the bundle, 3.75 GB free: the load needs 4 GB
	err := g.Materialize(context.Background(), spec)
	var noRoom *NoRoomError
	if !errors.As(err, &noRoom) {
		t.Fatalf("Materialize = %v, want a NoRoomError", err)
	}
	if noRoom.Needed != 4*gb1 || noRoom.Free != 4*gb1-gb1/4 {
		t.Fatalf("free %d needed %d", noRoom.Free, noRoom.Needed)
	}
	for _, want := range []string{"silico-toolbox:3", fmt.Sprint(noRoom.Free), fmt.Sprint(noRoom.Needed), "a12s-silico-silico-helix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if len(vm.loads) != 0 {
		t.Fatalf("loaded %v into a disk it doesn't fit", vm.loads)
	}
}

// A wake of a generation that already runs clears images an earlier switch left behind.
func TestMaterializePrunesOnAWakeOfTheSameGeneration(t *testing.T) {
	g, vm, spec := silicoVM(t, 20*gb1)
	spec.Generation, spec.Image = 2, "silico-toolbox:2"
	g.e.Local = engine.New(fakeClient(t, func() *fakeEngine {
		c := newFakeEngine(100 * gb1)
		c.images["sha256:tb2"] = &fakeImage{size: 2 * gb1, tags: []string{"silico-toolbox:2"}}
		c.images["sha256:bundle"] = &fakeImage{size: gb1 / 4}
		return c
	}()), "silico")
	if err := g.Materialize(context.Background(), spec); err != nil {
		t.Fatalf("Materialize = %v", err)
	}
	if got, want := vm.imageIDs(), []string{"sha256:vm-bundle", "sha256:vm-tb2"}; !slices.Equal(got, want) {
		t.Fatalf("images on the VM = %v, want %v", got, want)
	}
	if vm.containerLocked("old") == nil {
		t.Fatal("the running generation's container was replaced")
	}
}

func TestParseDF(t *testing.T) {
	got, err := parseDF("Filesystem     1024-blocks    Used Available Capacity Mounted on\noverlay           5000000 1000000   3999000      21% /\n")
	if err != nil || got != 3999000*1024 {
		t.Fatalf("= %d, %v", got, err)
	}
	if _, err := parseDF("df: /: No such file\n"); err == nil {
		t.Fatal("garbage parsed")
	}
}
