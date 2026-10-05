package main

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// putFleetRequest is api.PutFleetRequest plus Presets: preset tars keyed by
// the preset path as written in the Fleet. The Controller branch adds the
// same field to api.PutFleetRequest; once merged, the outer field shadows
// the embedded one with identical JSON, and this wrapper can go.
type putFleetRequest struct {
	api.PutFleetRequest
	Presets map[string][]byte `json:"presets,omitempty"`
}

func runUp(g *globals, args []string) error {
	fs := g.flags("up")
	asleep := fs.Bool("asleep", false, "create new Computers asleep")
	image := fs.String("controller-image", cli.DefaultControllerImage, "Controller image")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("up takes no arguments")
	}

	f, err := fleet.Load(g.fleetPath)
	if err != nil {
		return err
	}
	onDisk, err := fleet.LoadLock(g.lockPath())
	if err != nil {
		return err
	}
	lock := cloneLock(onDisk)
	b, err := cli.LoadBindings(g.bindings())
	if err != nil {
		return err
	}
	creds, err := b.Resolve(cli.Needed(f))
	if err != nil {
		return err
	}
	presets, err := g.presets(f, lock)
	if err != nil {
		return err
	}

	ctx, cancel := interruptible()
	defer cancel()
	c, err := cli.EnsureController(ctx, cli.StartOptions{
		Fleet: f.Metadata.Name,
		Image: *image,
		Log:   func(s string) { fmt.Fprintln(os.Stderr, s) },
	})
	if err != nil {
		return err
	}
	req := putFleetRequest{
		PutFleetRequest: api.PutFleetRequest{Fleet: f, Lock: lock, Credentials: creds, Asleep: *asleep},
		Presets:         presets,
	}
	var resp api.PutFleetResponse
	if err := c.Do(ctx, "PUT", "/v1/fleet", req, &resp); err != nil {
		return err
	}
	if resp.Lock != nil {
		lock = resp.Lock
	}
	if added := addedPins(onDisk, lock); len(added) > 0 {
		if err := lock.Save(g.lockPath()); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "pinned in %s: %s\n", fleet.LockFile, strings.Join(added, ", "))
	}
	for _, line := range cli.UpSummary(&resp, *asleep) {
		fmt.Println(line)
	}
	return nil
}

// presets tars every home.preset directory, keyed by the path as written in
// the Fleet, and pins each content hash in lock. A pinned preset whose
// content changed is an error: moving a pin is an explicit lock edit.
func (g *globals) presets(f *fleet.Fleet, lock *fleet.Lock) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, name := range f.ComputerNames() {
		p := f.Computers[name].Home.Preset
		if p == "" || out[p] != nil {
			continue
		}
		dir := g.presetDir(p)
		hash, err := fleet.HashPreset(dir)
		if err != nil {
			return nil, err
		}
		if pinned, ok := lock.Presets[p]; ok && pinned != hash {
			return nil, fmt.Errorf("preset %s changed since %s pinned it (%s, now %s); delete its entry from %s to re-pin",
				p, fleet.LockFile, pinned, hash, fleet.LockFile)
		}
		lock.PinPreset(p, hash)
		var buf bytes.Buffer
		if err := fleet.WritePresetTar(dir, &buf); err != nil {
			return nil, err
		}
		out[p] = buf.Bytes()
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func cloneLock(l *fleet.Lock) *fleet.Lock {
	return &fleet.Lock{Images: maps.Clone(l.Images), Bundles: maps.Clone(l.Bundles), Presets: maps.Clone(l.Presets)}
}

// addedPins lists lock keys in next that are not in prev ("slim",
// "claude-code@2.1.280", "./presets/scout"), or that differ from prev.
func addedPins(prev, next *fleet.Lock) []string {
	var out []string
	for _, pair := range [][2]map[string]string{
		{prev.Images, next.Images}, {prev.Bundles, next.Bundles}, {prev.Presets, next.Presets},
	} {
		var keys []string
		for k, v := range pair[1] {
			if old, ok := pair[0][k]; !ok || old != v {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		out = append(out, keys...)
	}
	return out
}
