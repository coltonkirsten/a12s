package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"sort"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/version"
)

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
	return g.up(*asleep, *image, nil)
}

// runLock is the only way to move a Service's pin on an unchanged ref.
func runLock(g *globals, args []string) error {
	fs := g.flags("lock")
	image := fs.String("controller-image", cli.DefaultControllerImage, "Controller image")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 || pos[0] != "update" {
		return usageError("lock update services/<svc>...")
	}
	f, err := fleet.Load(g.fleetPath)
	if err != nil {
		return err
	}
	var refresh []string
	for _, target := range pos[1:] {
		name, ok := strings.CutPrefix(target, "services/")
		if !ok {
			return usageError(fmt.Sprintf("%s: only services/<svc> pins move with lock update; image and bundle pins move by editing fleet.lock", target))
		}
		if !f.HasService(name) {
			return fmt.Errorf("no Service %s in %s", name, g.fleetPath)
		}
		refresh = append(refresh, name)
	}
	return g.up(false, *image, refresh)
}

func (g *globals) up(asleep bool, image string, refresh []string) error {
	f, err := fleet.Load(g.fleetPath)
	if err != nil {
		return err
	}
	onDisk, err := fleet.LoadLock(g.lockPath())
	if err != nil {
		return err
	}
	if err := checkA12sPin(onDisk, os.Stderr); err != nil {
		return err
	}
	lock := cloneLock(onDisk)
	if lock.A12s == "" && version.Release() {
		lock.A12s = version.Version
	}
	b, err := cli.LoadBindings(g.bindings())
	if err != nil {
		return err
	}
	creds, err := b.Resolve(cli.Needed(f))
	if err != nil {
		return err
	}
	svcCreds, err := b.ResolveBound(cli.NeededByServices(f))
	if err != nil {
		return err
	}
	maps.Copy(creds, svcCreds)
	presets, err := g.presets(f, lock)
	if err != nil {
		return err
	}

	ctx, cancel := interruptible()
	defer cancel()
	cli.PullMissing(ctx, cli.FleetImages(f, image), os.Stderr)
	c, err := cli.EnsureController(ctx, cli.StartOptions{
		Fleet: f.Metadata.Name,
		Image: image,
		Log:   func(s string) { fmt.Fprintln(os.Stderr, s) },
	})
	if err != nil {
		return err
	}
	for _, name := range refresh {
		delete(lock.Services, fleet.ServiceKey(name, f.Services[name].Image))
	}
	req := api.PutFleetRequest{Fleet: f, Lock: lock, Credentials: creds, Asleep: asleep, Presets: presets, Refresh: refresh}
	var resp api.PutFleetResponse
	if err := c.Do(ctx, "PUT", "/v1/fleet", req, &resp); err != nil {
		return err
	}
	return g.finishUp(onDisk, lock, &resp, asleep, os.Stdout, os.Stderr)
}

// finishUp keeps what converged applied and pinned; any failure still fails the command.
func (g *globals) finishUp(onDisk, lock *fleet.Lock, resp *api.PutFleetResponse, asleep bool, stdout, stderr io.Writer) error {
	if resp.Lock != nil {
		resp.Lock.A12s = lock.A12s
		lock = resp.Lock
	}
	if added := addedPins(onDisk, lock); len(added) > 0 {
		if err := lock.Save(g.lockPath()); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "pinned in %s: %s\n", fleet.LockFile, strings.Join(added, ", "))
	}
	failed := cli.UpErrors(resp)
	if g.json {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return err
		}
		if len(failed) > 0 {
			return exitCode(1)
		}
		return nil
	}
	for _, line := range cli.UpSummary(resp, asleep) {
		fmt.Fprintln(stdout, line)
	}
	for _, line := range failed {
		fmt.Fprintln(stderr, line)
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d Computer(s) or Service(s) did not converge (a12s status shows their state)", len(failed))
	}
	return nil
}

// presets re-pins a preset whose content changed: it is the project's own source, versioned with the Fleet file.
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
		lock.SetPreset(p, hash)
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
	return &fleet.Lock{A12s: l.A12s, Images: maps.Clone(l.Images), Bundles: maps.Clone(l.Bundles), Presets: maps.Clone(l.Presets), Services: maps.Clone(l.Services)}
}

func addedPins(prev, next *fleet.Lock) []string {
	var out []string
	if next.A12s != prev.A12s {
		out = append(out, "a12s@"+next.A12s)
	}
	for _, pair := range [][2]map[string]string{
		{prev.Images, next.Images}, {prev.Bundles, next.Bundles}, {prev.Presets, next.Presets}, {prev.Services, next.Services},
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

// checkA12sPin keeps a Fleet on the a12s release its lock names: images, the Controller and computerd all come from
// that release, so a different binary would run a mix.
func checkA12sPin(l *fleet.Lock, stderr io.Writer) error {
	switch {
	case l.A12s == "" || l.A12s == version.Version:
		return nil
	case !version.Release():
		fmt.Fprintf(stderr, "warning: %s pins a12s %s; this is a source build (dev) using local images\n", fleet.LockFile, l.A12s)
		return nil
	}
	return fmt.Errorf("%s pins a12s %s, but this binary is %s\n  fix: install a12s %s, or to upgrade the Fleet set `a12s: %s` in %s and run a12s up",
		fleet.LockFile, l.A12s, version.Version, l.A12s, version.Version, fleet.LockFile)
}
