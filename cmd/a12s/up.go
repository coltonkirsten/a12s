package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

type upOptions struct {
	asleep, force, memoryOnly bool
	image                     string
	refresh                   []string
}

func runUp(g *globals, args []string) error {
	fs := g.flags("up")
	var o upOptions
	fs.BoolVar(&o.asleep, "asleep", false, "create new agent computers asleep")
	fs.BoolVar(&o.force, "force", false, "overwrite changes made through the API since the last up without asking")
	fs.BoolVar(&o.memoryOnly, "memory-only-credentials", false, "a new Controller keeps credential values in memory only")
	fs.StringVar(&o.image, "controller-image", cli.DefaultControllerImage, "Controller image")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("up takes no arguments")
	}
	return g.up(o)
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
	return g.up(upOptions{image: *image, refresh: refresh})
}

// loadSpec reads the Fleet file as the API takes it: presets and seccomp profiles inline.
func (g *globals) loadSpec() (*fleet.Spec, *fleet.Fleet, error) {
	spec, err := fleet.LoadSpec(g.fleetPath)
	if err != nil {
		return nil, nil, err
	}
	if err := g.inline(spec); err != nil {
		return nil, nil, err
	}
	f, err := spec.Resolve()
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", g.fleetPath, err)
	}
	if spec.LegacyKey {
		fmt.Fprintf(os.Stderr, "hint: %s uses computers:, the old name of agent_computers: (it still works in this release)\n", g.fleetPath)
	}
	return spec, f, nil
}

func (g *globals) up(o upOptions) error {
	asleep, image, refresh := o.asleep, o.image, o.refresh
	spec, f, err := g.loadSpec()
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
	// Unbound names may already be in the Controller's store; it refuses what is set nowhere.
	creds, err := b.ResolveBound(cli.Needed(f))
	if err != nil {
		return err
	}
	svcCreds, err := b.ResolveBound(cli.NeededByServices(f))
	if err != nil {
		return err
	}
	maps.Copy(creds, svcCreds)

	ctx, cancel := interruptible()
	defer cancel()
	cli.PullMissing(ctx, cli.FleetImages(f, image), os.Stderr)
	c, err := cli.EnsureController(ctx, cli.StartOptions{
		Fleet:                 f.Metadata.Name,
		Image:                 image,
		Log:                   func(s string) { fmt.Fprintln(os.Stderr, s) },
		MemoryOnlyCredentials: o.memoryOnly,
	})
	if err != nil {
		return err
	}
	for _, name := range refresh {
		delete(lock.Services, fleet.ServiceKey(name, f.Services[name].Image))
	}
	req := api.PutFleetRequest{Fleet: spec, Lock: lock, Credentials: creds, Asleep: asleep, Refresh: refresh, File: true}
	version, err := g.checkDrift(ctx, c, req, o.force)
	if err != nil {
		return err
	}
	var resp api.PutFleetResponse
	if err := c.DoHeaders(ctx, "PUT", "/v1/fleet", map[string]string{"If-Match": fmt.Sprintf("%q", fmt.Sprint(version))}, req, &resp); err != nil {
		if cli.HasCode(err, api.ErrVersionMismatch) {
			return fmt.Errorf("%w\n  the Fleet changed while a12s up ran; run it again", err)
		}
		return err
	}
	if err := g.finishUp(onDisk, lock, &resp, asleep, os.Stdout, os.Stderr); err != nil {
		return err
	}
	if !g.json {
		for _, line := range cli.SeccompLines(f) {
			fmt.Fprintln(os.Stdout, line)
		}
	}
	return nil
}

// checkDrift finds what this up would undo of the changes made through the API since the last up, and asks before
// overwriting them: y/N in a terminal, a failure elsewhere unless force. It returns the version the write is
// conditional on.
func (g *globals) checkDrift(ctx context.Context, c *cli.Client, req api.PutFleetRequest, force bool) (int64, error) {
	var live api.FleetResponse
	if err := c.Do(ctx, "GET", "/v1/fleet", nil, &live); err != nil {
		return 0, err
	}
	if len(live.Drift) == 0 {
		return live.Version, nil
	}
	var dry api.PutFleetResponse
	if err := c.Do(ctx, "PUT", "/v1/fleet?dryRun=true", req, &dry); err != nil {
		return 0, err
	}
	drifted := map[string]bool{}
	for _, d := range live.Drift {
		drifted[d.Kind+"/"+d.Name] = true
	}
	var undo []api.Change
	for _, ch := range dry.Changes {
		if drifted[ch.Kind+"/"+ch.Name] {
			undo = append(undo, ch)
		}
	}
	if len(undo) == 0 {
		return live.Version, nil
	}
	fmt.Fprintf(os.Stderr, "the live Fleet changed since the last a12s up; this up undoes:\n")
	for _, line := range cli.ChangeLines(undo) {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	switch {
	case force:
		return live.Version, nil
	case isTerminal(os.Stdin):
		fmt.Fprint(os.Stderr, "apply anyway? [y/N] ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a == "y" || a == "yes" {
			return live.Version, nil
		}
		return 0, errors.New("nothing applied")
	}
	return 0, errors.New("nothing applied: the live Fleet has changes this up would undo\n  fix: a12s export to keep them in the file, or a12s up --force to overwrite them")
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

// inline turns what only a Fleet file can say into what the API takes: each home.preset directory becomes files and
// each security.seccomp path the profile itself.
func (g *globals) inline(spec *fleet.Spec) error {
	var defaultKind string
	one := func(field string, raw json.RawMessage) (json.RawMessage, error) {
		if len(raw) == 0 {
			return raw, nil
		}
		var c fleet.Computer
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		kind := c.Harness.Kind
		if kind == "" {
			kind = defaultKind
		}
		if p := c.Home.Preset; p != "" {
			files, err := fleet.InlineFiles(g.presetDir(p))
			if err != nil {
				return nil, fmt.Errorf("%s.home.preset: %w", field, err)
			}
			// The preset's instructions file is the instructions, so a preset and inline instructions read the same.
			name := fleet.InstructionsFile(kind)
			if body, ok := files[name]; ok {
				if c.Instructions != "" {
					return nil, fmt.Errorf("%s.instructions: the preset %s also has %s; keep one", field, p, name)
				}
				c.Instructions = body
				delete(files, name)
			}
			for path, body := range files {
				if _, dup := c.Files[path]; dup {
					return nil, fmt.Errorf("%s.files: %s is also in the preset %s; keep one", field, path, p)
				}
				if c.Files == nil {
					c.Files = map[string]string{}
				}
				c.Files[path] = body
			}
			c.Home.Preset = ""
		}
		if p := c.Security.Seccomp.Path; p != "" {
			data, err := os.ReadFile(g.presetDir(p))
			if err != nil {
				return nil, fmt.Errorf("%s.security.seccomp: %v", field, err)
			}
			if _, err := fleet.ValidateSeccomp(data); err != nil {
				return nil, fmt.Errorf("%s.security.seccomp: %s: %v", field, p, err)
			}
			if err := c.Security.Seccomp.UnmarshalJSON(data); err != nil {
				return nil, fmt.Errorf("%s.security.seccomp: %s: %v", field, p, err)
			}
		}
		if field == "defaults" {
			defaultKind = c.Harness.Kind
		}
		return fleet.SpecOf(c), nil
	}
	var err error
	if spec.Defaults, err = one("defaults", spec.Defaults); err != nil {
		return err
	}
	for _, name := range spec.Names() {
		if spec.AgentComputers[name], err = one("agent_computers."+name, spec.AgentComputers[name]); err != nil {
			return err
		}
	}
	return nil
}

func cloneLock(l *fleet.Lock) *fleet.Lock {
	return &fleet.Lock{A12s: l.A12s, Images: maps.Clone(l.Images), Bundles: maps.Clone(l.Bundles), Services: maps.Clone(l.Services)}
}

func addedPins(prev, next *fleet.Lock) []string {
	var out []string
	if next.A12s != prev.A12s {
		out = append(out, "a12s@"+next.A12s)
	}
	for _, pair := range [][2]map[string]string{
		{prev.Images, next.Images}, {prev.Bundles, next.Bundles}, {prev.Services, next.Services},
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
