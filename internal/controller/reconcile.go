package controller

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/images/browser"
	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// Default image references (PLAN "HarnessBundle image").
func imageRef(image string) string {
	switch image {
	case fleet.TierSlim, fleet.TierBrowser:
		return "a12s/" + image + ":dev"
	}
	return image
}

func bundleRef(h fleet.Harness) string { return "a12s/bundle-" + h.Kind + ":" + h.Version }

// PutFleet is `a12s up`: admit the Fleet, resolve and pin what the lock
// lacks, then converge every Computer. Admission errors change nothing.
func (c *Controller) PutFleet(ctx context.Context, req api.PutFleetRequest) (api.PutFleetResponse, error) {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()

	f := req.Fleet
	if f == nil {
		return api.PutFleetResponse{}, errorf(http.StatusBadRequest, api.ErrInvalid, "fleet is required")
	}
	if err := f.Validate(); err != nil {
		return api.PutFleetResponse{}, errorf(http.StatusBadRequest, api.ErrInvalid, "%v", err)
	}
	if f.Metadata.Name != c.cfg.Fleet {
		return api.PutFleetResponse{}, errorf(http.StatusBadRequest, api.ErrInvalid, "this Controller runs fleet %q, not %q", c.cfg.Fleet, f.Metadata.Name)
	}
	creds := req.Credentials
	c.mu.Lock()
	if len(creds) == 0 {
		creds = c.creds
	}
	c.mu.Unlock()
	for _, name := range f.ComputerNames() {
		if missing := missingCredentials(f.Computers[name].Credentials, creds); len(missing) > 0 {
			return api.PutFleetResponse{}, errorf(http.StatusBadRequest, api.ErrInvalid,
				"computers.%s: no material for credentials %s (check the bindings file)", name, strings.Join(missing, ", "))
		}
	}
	lock, hosts, err := c.resolve(ctx, f, req.Lock, req.Presets)
	if err != nil {
		return api.PutFleetResponse{}, err
	}

	// Admitted: this Fleet is now the desired state.
	c.mu.Lock()
	var previous map[string]fleet.Computer
	if c.state.Fleet != nil {
		previous = c.state.Fleet.Computers
	}
	c.state.Fleet, c.state.Lock = f, lock
	maps.Copy(c.state.BundleHosts, hosts)
	c.creds = maps.Clone(creds)
	for _, name := range f.ComputerNames() {
		rec := c.state.Computers[name]
		if rec == nil {
			rec = &Record{DesiredPower: driver.PowerAwake}
			if req.Asleep {
				rec.DesiredPower = driver.PowerAsleep
			}
			c.state.Computers[name] = rec
		}
		rec.Restarts = 0
	}
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return api.PutFleetResponse{}, err
	}
	prunePresets(c.cfg.StateDir, lock.Presets)
	if err := c.syncInboxes(ctx, f, api.PrincipalLocal); err != nil {
		return api.PutFleetResponse{}, err
	}

	res := api.PutFleetResponse{Lock: lock, Created: []string{}, Updated: []string{}, Staged: []string{}, Removed: []string{}, Warnings: f.InboxWarnings()}
	obs, err := c.drv.Observe(ctx)
	if err != nil {
		return res, err
	}
	for _, d := range f.DriveNames() {
		spec := driver.DriveSpec{ID: driver.WorkDriveID(d), Kind: driver.DriveWork, Size: int64(f.Drives[d].Size)}
		if _, err := c.drv.EnsureDrive(ctx, spec); err != nil {
			return res, err
		}
	}

	gone := map[string]bool{}
	for name := range previous {
		gone[name] = true
	}
	for name := range obs.Computers {
		gone[name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(gone)) {
		if _, ok := f.Computers[name]; ok {
			continue
		}
		if err := c.removeComputer(ctx, name); err != nil {
			return res, err
		}
		res.Removed = append(res.Removed, name)
	}

	outcomes := c.convergeAll(ctx, f.ComputerNames(), obs)
	for _, name := range f.ComputerNames() {
		o := outcomes[name]
		_, existed := previous[name]
		switch {
		case o.err != nil:
			if res.Errors == nil {
				res.Errors = map[string]string{}
			}
			res.Errors[name] = o.err.Error()
			if !existed {
				res.Created = append(res.Created, name)
			}
		case !existed:
			res.Created = append(res.Created, name)
		case o.result == staged:
			res.Staged = append(res.Staged, name)
		case o.result == restaged:
			res.Updated = append(res.Updated, name)
		}
	}
	c.refreshAddrs(ctx)
	return res, nil
}

type result int

const (
	unchanged result = iota
	restaged
	staged
)

type outcome struct {
	result result
	err    error
}

// convergeAll converges Computers in parallel; each reports as soon as it
// is done, whatever the others do. applyMu must be held exclusively.
func (c *Controller) convergeAll(ctx context.Context, names []string, obs driver.Observation) map[string]outcome {
	var mu sync.Mutex
	out := map[string]outcome{}
	var wg sync.WaitGroup
	for _, name := range names {
		st, ok := obs.Computers[name]
		wg.Go(func() {
			r, err := c.converge(ctx, name, st, ok)
			mu.Lock()
			out[name] = outcome{r, err}
			mu.Unlock()
		})
	}
	wg.Wait()
	return out
}

// converge brings one Computer to the applied Fleet: unchanged ones only
// get their power state enforced; a changed one is restaged now, or staged
// for its next idle boundary when it is busy. The caller holds applyMu
// exclusively or the Computer's lock.
func (c *Controller) converge(ctx context.Context, name string, obs driver.ComputerState, exists bool) (result, error) {
	spec, hash, err := c.buildSpec(name)
	if err != nil {
		return unchanged, c.fail(name, err, false)
	}
	c.mu.Lock()
	rec := c.state.Computers[name]
	homeSize := c.state.Fleet.Computers[name].Home.Size
	c.mu.Unlock()
	if _, err := c.drv.EnsureDrive(ctx, driver.DriveSpec{ID: spec.HomeDrive, Kind: driver.DriveHome, Computer: name, Size: int64(homeSize)}); err != nil {
		return unchanged, c.fail(name, err, true)
	}

	running := exists && obs.Power == driver.PowerAwake
	if exists && obs.Generation == rec.Generation && hash == rec.SpecHash {
		c.mu.Lock()
		if rec.StagedGeneration != 0 { // the Fleet went back to what runs
			rec.StagedGeneration, rec.StagedHash = 0, ""
		}
		// Same spec hash, so the same egress, bundle and drives: this only
		// fills in state written before Active existed.
		rec.Active = c.activeLocked(name)
		_ = c.saveLocked()
		c.mu.Unlock()
		return unchanged, c.ensurePower(ctx, name, running)
	}
	if running {
		// Closed before the idle check, so no prompt slips in between.
		c.closeAdmission(name)
		if c.busy(ctx, name) {
			c.openAdmission(name)
			return staged, c.stageLater(name, hash, obs.Generation, "applies when the current turn ends")
		}
	}
	err = c.restage(ctx, name)
	var held *driveHeld
	if errors.As(err, &held) {
		c.openAdmission(name)
		err := c.stageLater(name, hash, obs.Generation, held.Error())
		c.retryIfFreed(ctx, name)
		return staged, err
	}
	return restaged, err
}

// retryIfFreed re-checks, after the Computer's staged generation is
// recorded, whether the drive it waits for is still held. A holder kicks
// pending Computers only after its detachment is observed, so if that kick
// came before this Computer was recorded pending, this check sees the
// drive free and retries: no wakeup is lost.
func (c *Controller) retryIfFreed(ctx context.Context, name string) {
	if held, err := c.exclusiveHolder(ctx, name); err == nil && held == nil {
		c.spawn(func() { c.applyStaged(name) })
	}
}

// stageLater records a generation to apply at the Computer's next idle
// boundary, or once a drive it needs is released.
func (c *Controller) stageLater(name, hash string, observed int64, why string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.state.Computers[name]
	if rec.StagedHash != hash {
		rec.StagedGeneration = max(rec.Generation, rec.StagedGeneration, observed) + 1
		rec.StagedHash = hash
	}
	c.logf("%s: generation %d staged; %s", name, rec.StagedGeneration, why)
	return c.saveLocked()
}

// activeLocked is the configuration the Computer's current Fleet entry
// makes active when its generation is staged.
func (c *Controller) activeLocked(name string) Active {
	comp := c.state.Fleet.Computers[name]
	return Active{
		Allow:       slices.Clone(comp.Egress.Allow),
		SignInHosts: slices.Clone(c.state.BundleHosts[comp.Harness.BundleKey()]),
		Drives:      slices.Clone(comp.Drives),
		Unenforced:  c.state.Fleet.Egress.Unenforced(),
	}
}

// driveHeld means an exclusive drive the new generation attaches is still
// attached to another Computer (whose own change waits for its idle
// boundary). It is a dependency, not a failure.
type driveHeld struct{ drive, holder string }

func (e *driveHeld) Error() string {
	return "waits for " + e.holder + " to release exclusive drive " + e.drive
}

// exclusiveHolder returns the first exclusive drive of the Computer's
// Fleet entry that another Computer has attached, as observed.
func (c *Controller) exclusiveHolder(ctx context.Context, name string) (*driveHeld, error) {
	c.mu.Lock()
	f := c.state.Fleet
	comp := f.Computers[name]
	c.mu.Unlock()
	var exclusive []string
	for _, m := range comp.Drives {
		if f.Drives[m.Name].Access == fleet.AccessExclusive {
			exclusive = append(exclusive, m.Name)
		}
	}
	if len(exclusive) == 0 {
		return nil, nil
	}
	obs, err := c.drv.Observe(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range exclusive {
		for _, other := range obs.Drives[driver.WorkDriveID(d)].AttachedTo {
			if other != name {
				return &driveHeld{drive: d, holder: other}, nil
			}
		}
	}
	return nil, nil
}

// holdsObsolete reports whether the active generation attaches an
// exclusive drive the Computer's Fleet entry no longer attaches.
func (c *Controller) holdsObsolete(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, rec := c.state.Fleet, c.state.Computers[name]
	comp := f.Computers[name]
	for _, old := range rec.Active.Drives {
		if d, ok := f.Drives[old.Name]; ok && d.Access != fleet.AccessExclusive {
			continue
		}
		if !slices.ContainsFunc(comp.Drives, func(m fleet.DriveMount) bool { return m.Name == old.Name }) {
			return true
		}
	}
	return false
}

// release stops and removes an idle Computer's old generation, which
// detaches its drives, and reads that back; its new generation stays
// staged until the drives it needs are free. The caller has closed
// admission and found the Computer idle.
func (c *Controller) release(ctx context.Context, name string, obs driver.ComputerState) error {
	c.setTransition(name, true)
	defer c.setTransition(name, false)
	c.mu.Lock()
	c.rtLocked(name).released = true
	c.mu.Unlock()
	if obs.Power == driver.PowerAwake {
		c.setPhase(name, api.PhaseStopping, nil)
		c.finalStatus(ctx, name)
		c.stopEvents(name)
		if err := c.stop(ctx, name, obs.Generation); err != nil {
			return c.fail(name, err, true)
		}
	}
	if err := c.removeNetworked(ctx, name, "release of the old generation"); err != nil {
		return c.fail(name, err, true)
	}
	c.setPhase(name, api.PhaseProvisioning, nil)
	c.logf("%s: generation %d removed to release its drives (drives kept)", name, obs.Generation)
	c.kickPending(name)
	return nil
}

// kickPending retries staged generations, for example one that waited for
// a drive the just-changed Computer released.
func (c *Controller) kickPending(except string) {
	c.mu.Lock()
	var names []string
	for name, rec := range c.state.Computers {
		if name != except && rec.pending() {
			names = append(names, name)
		}
	}
	c.mu.Unlock()
	for _, name := range names {
		c.spawn(func() { c.applyStaged(name) })
	}
}

// ensurePower enforces the desired power of an up-to-date Computer.
func (c *Controller) ensurePower(ctx context.Context, name string, running bool) error {
	c.mu.Lock()
	rec := c.state.Computers[name]
	r := c.rtLocked(name)
	desired, phase := rec.DesiredPower, r.phase
	// After a Controller restart nothing is known to be injected: material
	// that is present and not known to be in the Computer is injected.
	material := c.credsHashLocked(c.state.Fleet.Computers[name].Credentials)
	rotate := material != "" && r.credsHash != material
	c.mu.Unlock()
	switch {
	case desired == driver.PowerAsleep && running:
		return c.sleep(ctx, name)
	case desired == driver.PowerAsleep:
		c.setPhase(name, api.PhaseAsleep, nil)
		return nil
	case !running:
		return c.start(ctx, name)
	case phase != api.PhaseReady && phase != api.PhaseBusy:
		if err := c.attach(ctx, name); err != nil {
			return c.start(ctx, name)
		}
		if !rotate {
			return nil
		}
		fallthrough
	case rotate:
		// New material for a running Computer: computerd restarts the
		// harness on its next turn when .generation changes.
		if err := c.inject(ctx, name); err != nil {
			return c.fail(name, err, true)
		}
		c.mu.Lock()
		r.credsHash = c.credsHashLocked(c.state.Fleet.Computers[name].Credentials)
		c.mu.Unlock()
		c.logf("%s: credentials re-injected", name)
	}
	return nil
}

// busy asks a running Computer's computerd whether a turn is running. A
// computerd that does not answer may be running one: unknown is busy (only
// `apply --interrupt` overrides it). The cached phase is never trusted:
// a just-accepted turn's events may not have arrived.
func (c *Controller) busy(ctx context.Context, name string) bool {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := c.client(name).Status(sctx)
	if err != nil {
		return true
	}
	return st.ActiveTurn != "" || st.Phase == "busy"
}

// idle reports whether the Computer runs no turn: it is not running, or
// its computerd says so.
func (c *Controller) idle(ctx context.Context, name string) bool {
	obs, exists, err := c.observeOne(ctx, name)
	if err != nil {
		return false
	}
	if !exists || !obs.Running {
		return true
	}
	return !c.busy(ctx, name)
}

// restage stops the Computer if it runs, stages a new generation of the
// applied spec, and starts it unless it should sleep. The caller holds
// applyMu exclusively or the Computer's lock.
func (c *Controller) restage(ctx context.Context, name string) error {
	spec, hash, err := c.buildSpec(name)
	if err != nil {
		return c.fail(name, err, false)
	}
	obs, exists, err := c.observeOne(ctx, name)
	if err != nil {
		return c.fail(name, err, true)
	}
	if held, err := c.exclusiveHolder(ctx, name); err != nil {
		return c.fail(name, err, true)
	} else if held != nil {
		// The holder may itself wait for a drive this Computer attaches
		// (a swap): release that one first, so the holder can proceed.
		if exists && c.holdsObsolete(name) {
			if err := c.release(ctx, name, obs); err != nil {
				return err
			}
		}
		return held
	}
	c.mu.Lock()
	preset := c.state.Fleet.Computers[name].Home.Preset
	sum := c.state.Lock.Presets[preset]
	c.mu.Unlock()
	if preset != "" {
		if spec.Preset = loadPreset(c.cfg.StateDir, sum); spec.Preset == nil {
			return c.fail(name, fmt.Errorf("preset %s (%s) is not stored; run a12s up to send it", preset, sum), true)
		}
	}
	c.setTransition(name, true)
	defer c.setTransition(name, false)
	c.mu.Lock()
	rec := c.state.Computers[name]
	// A staged generation keeps its number; any other change gets a new one.
	spec.Generation = max(rec.Generation, obs.Generation) + 1
	if rec.pending() && rec.StagedHash == hash {
		spec.Generation = max(spec.Generation, rec.StagedGeneration)
	} else {
		spec.Generation = max(spec.Generation, rec.StagedGeneration+1)
	}
	c.mu.Unlock()

	c.closeAdmission(name)
	if exists && obs.Power == driver.PowerAwake {
		c.setPhase(name, api.PhaseStopping, nil)
		c.finalStatus(ctx, name)
		c.stopEvents(name)
		if err := c.stop(ctx, name, obs.Generation); err != nil {
			return c.fail(name, err, true)
		}
	}
	c.setPhase(name, api.PhaseProvisioning, nil)
	if err := c.drv.StageComputer(ctx, spec); err != nil {
		return c.fail(name, err, true)
	}
	err = c.confirm(ctx, name, fmt.Sprintf("generation %d staged", spec.Generation), func(st driver.ComputerState, exists bool) bool {
		return exists && st.Generation == spec.Generation && !st.Running
	})
	if err != nil {
		return c.fail(name, err, true)
	}
	c.mu.Lock()
	rec.Generation, rec.SpecHash = spec.Generation, hash
	rec.StagedGeneration, rec.StagedHash = 0, ""
	// The new generation's egress policy and drives take effect now.
	rec.Active = c.activeLocked(name)
	c.rtLocked(name).released = false
	desired := rec.DesiredPower
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	c.logf("%s: generation %d staged", name, spec.Generation)
	c.setTransition(name, false)
	c.kickPending(name)
	if desired == driver.PowerAwake {
		return c.start(ctx, name)
	}
	c.setPhase(name, api.PhaseAsleep, nil)
	return nil
}

// applyStaged applies a staged generation at an idle boundary.
func (c *Controller) applyStaged(name string) {
	defer c.lockComputer(name)()
	c.mu.Lock()
	rec := c.state.Computers[name]
	due := rec != nil && rec.pending()
	c.mu.Unlock()
	if !due {
		return
	}
	// No holder check here: restage itself waits for a held drive, and
	// first releases one this Computer holds that the holder needs (a swap
	// whose turns have both ended).
	c.closeAdmission(name)
	if !c.idle(c.ctx, name) {
		c.openAdmission(name)
		return
	}
	err := c.restage(c.ctx, name)
	if err != nil {
		c.openAdmission(name)
	}
	c.kick(name)
	var held *driveHeld
	if errors.As(err, &held) {
		c.retryIfFreed(c.ctx, name)
	}
}

// Apply is `a12s apply [--interrupt --grace D]`: apply staged generations
// now on idle Computers and, with interrupt, on busy ones after stopping
// their turn and waiting up to grace.
func (c *Controller) Apply(ctx context.Context, req api.ApplyRequest) (api.ApplyResponse, error) {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	res := api.ApplyResponse{Applied: []string{}, Pending: []string{}}
	c.mu.Lock()
	var names []string
	for name, rec := range c.state.Computers {
		if rec.pending() {
			names = append(names, name)
		}
	}
	c.mu.Unlock()
	sort.Strings(names)
	applied := make([]bool, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			c.closeAdmission(name)
			if !c.idle(ctx, name) {
				if !req.Interrupt {
					c.openAdmission(name)
					return
				}
				c.logf("%s: interrupting the active turn to apply the staged generation", name)
				c.interrupt(ctx, name, time.Duration(req.Grace))
			}
			err := c.restage(ctx, name)
			var held *driveHeld
			if errors.As(err, &held) {
				c.openAdmission(name)
				c.logf("%s: %v", name, err)
				c.retryIfFreed(ctx, name)
				return
			}
			errs[i] = err
			applied[i] = err == nil
		})
	}
	wg.Wait()
	for i, name := range names {
		if applied[i] {
			res.Applied = append(res.Applied, name)
		} else if errs[i] == nil {
			res.Pending = append(res.Pending, name)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return res, err
	}
	return res, nil
}

// SetPower is `a12s sleep` and `a12s wake`.
func (c *Controller) SetPower(ctx context.Context, name string, req api.PowerRequest) error {
	defer c.lockComputer(name)()
	c.mu.Lock()
	rec := c.state.Computers[name]
	inFleet := c.state.Fleet != nil && rec != nil
	if inFleet {
		_, inFleet = c.state.Fleet.Computers[name]
	}
	if inFleet && req.State == api.PowerAwake {
		maps.Copy(c.creds, req.Credentials)
		rec.DesiredPower = driver.PowerAwake
		rec.Restarts = 0
		_ = c.saveLocked()
	}
	c.mu.Unlock()
	if !inFleet {
		return errorf(http.StatusNotFound, api.ErrNotFound, "no computer %q in the Fleet", name)
	}
	switch req.State {
	case api.PowerAsleep:
		return c.sleep(ctx, name)
	case api.PowerAwake:
		obs, ok, err := c.observeOne(ctx, name)
		if err != nil {
			return err
		}
		_, err = c.converge(ctx, name, obs, ok)
		var cm *credentialsMissing
		if errors.As(err, &cm) {
			return errorf(http.StatusBadRequest, api.ErrInvalid, "%v", err)
		}
		return err
	}
	return errorf(http.StatusBadRequest, api.ErrInvalid, "state must be %q or %q", api.PowerAwake, api.PowerAsleep)
}

// removeComputer stops and deletes a Computer the Fleet no longer
// declares. Its drives stay (status lists them as orphans).
func (c *Controller) removeComputer(ctx context.Context, name string) error {
	c.setTransition(name, true)
	c.stopEvents(name)
	if err := c.removeNetworked(ctx, name, "removal"); err != nil {
		c.setTransition(name, false)
		return err
	}
	c.mu.Lock()
	delete(c.state.Computers, name)
	delete(c.rt, name)
	delete(c.clients, name)
	err := c.saveLocked()
	c.mu.Unlock()
	c.logf("%s: removed (drives kept)", name)
	c.kickPending(name)
	return err
}

// resolve checks the Fleet against the substrate and the lock, makes the
// images and bundles available, and returns the lock with missing pins
// added (an existing pin never changes) and each bundle's sign-in hosts.
func (c *Controller) resolve(ctx context.Context, f *fleet.Fleet, in *fleet.Lock, presets map[string][]byte) (*fleet.Lock, map[string][]string, error) {
	invalid := func(format string, args ...any) error {
		return errorf(http.StatusBadRequest, api.ErrInvalid, format, args...)
	}
	caps, err := c.drv.Capabilities(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !caps.EgressEnforcement && !f.Egress.Unenforced() {
		return nil, nil, invalid("this substrate cannot enforce egress; set egress.enforcement: unenforced to run anyway")
	}
	for _, d := range f.DriveNames() {
		if f.Drives[d].Access == fleet.AccessSharedWrite && !caps.SharedWriteDrives {
			return nil, nil, invalid("drives.%s: this substrate has no shared-write storage", d)
		}
	}

	lock := fleet.NewLock()
	if in != nil {
		maps.Copy(lock.Images, in.Images)
		maps.Copy(lock.Bundles, in.Bundles)
		maps.Copy(lock.Presets, in.Presets)
	}
	hosts := map[string][]string{}
	seen := map[string]bool{}
	for _, name := range f.ComputerNames() {
		comp := f.Computers[name]
		if !seen["image "+comp.Image] {
			seen["image "+comp.Image] = true
			id, err := ensurePinned(ctx, imageRef(comp.Image), lock.Images[comp.Image], c.drv.EnsureImage)
			if err != nil {
				return nil, nil, fmt.Errorf("computers.%s.image: %w", name, err)
			}
			lock.PinImage(comp.Image, id)
		}
		if key := comp.Harness.BundleKey(); hosts[key] == nil {
			var b driver.Bundle
			id, err := ensurePinned(ctx, bundleRef(comp.Harness), lock.Bundles[key], func(ctx context.Context, ref string) (string, error) {
				var err error
				b, err = c.drv.EnsureBundle(ctx, ref)
				return b.ImageID, err
			})
			if err != nil {
				return nil, nil, fmt.Errorf("computers.%s.harness: %w", name, err)
			}
			m := b.Manifest
			if m.Kind != comp.Harness.Kind || m.Version != comp.Harness.Version {
				return nil, nil, invalid("computers.%s.harness: bundle %s is %s@%s", name, id, m.Kind, m.Version)
			}
			if m.Arch != "" && !slices.Contains(caps.Architectures, m.Arch) {
				return nil, nil, invalid("computers.%s.harness: bundle is for %s, the substrate runs %s", name, m.Arch, strings.Join(caps.Architectures, ", "))
			}
			lock.PinBundle(m.Kind, m.Version, id)
			hosts[key] = append([]string{}, m.SignInHosts...)
		}
		if p := comp.Home.Preset; p != "" && !seen["preset "+p] {
			seen["preset "+p] = true
			data, ok := presets[p]
			if !ok {
				return nil, nil, invalid("computers.%s.home.preset: %q was not sent with the Fleet", name, p)
			}
			sum, err := hashPresetTar(data)
			if err != nil {
				return nil, nil, invalid("computers.%s.home.preset: %v", name, err)
			}
			if pin := lock.Presets[p]; pin != "" && pin != sum {
				return nil, nil, invalid("preset %s changed since fleet.lock pinned it (%s, now %s); remove the pin to accept it", p, pin, sum)
			}
			if err := savePreset(c.cfg.StateDir, sum, data); err != nil {
				return nil, nil, err
			}
			lock.PinPreset(p, sum)
		}
	}
	return lock, hosts, nil
}

// ensurePinned makes ref available and returns its image ID. When the lock
// pins an ID, that ID is what runs: it must be available locally, or ref
// must resolve to it.
func ensurePinned(ctx context.Context, ref, pin string, ensure func(context.Context, string) (string, error)) (string, error) {
	if pin == "" {
		return ensure(ctx, ref)
	}
	if id, err := ensure(ctx, pin); err == nil && id == pin {
		return id, nil
	}
	id, err := ensure(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("fleet.lock pins %s to %s, which is not available: %w", ref, pin, err)
	}
	if id != pin {
		return "", errorf(http.StatusBadRequest, api.ErrInvalid, "fleet.lock pins %s to %s, which is not available here (%s is %s); build or pull the pinned image, or edit fleet.lock", ref, pin, ref, id)
	}
	return id, nil
}

// buildSpec is the Driver spec of a Computer under the applied Fleet and
// Lock (generation and preset unset), and the hash of what makes a
// generation: image, bundle, resources, drives, environment, egress and
// the Fleet's egress enforcement.
func (c *Controller) buildSpec(name string) (driver.ComputerSpec, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, lock := c.state.Fleet, c.state.Lock
	if f == nil || lock == nil {
		return driver.ComputerSpec{}, "", errors.New("no Fleet applied")
	}
	comp, ok := f.Computers[name]
	if !ok {
		return driver.ComputerSpec{}, "", fmt.Errorf("no computer %q in the Fleet", name)
	}
	image, bundle := lock.Images[comp.Image], lock.Bundles[comp.Harness.BundleKey()]
	if image == "" || bundle == "" {
		return driver.ComputerSpec{}, "", errors.New("image or bundle is not pinned; run a12s up")
	}
	spec := driver.ComputerSpec{
		Name:        name,
		Image:       image,
		Tier:        comp.Tier(),
		Bundle:      driver.BundleRef{Kind: comp.Harness.Kind, Version: comp.Harness.Version, Image: bundle},
		Model:       comp.Harness.Model,
		Autonomy:    comp.Autonomy,
		CPUMillis:   comp.Resources.CPU.Millis(),
		Memory:      int64(comp.Resources.Memory),
		HomeDrive:   driver.HomeDriveID(name),
		Credentials: comp.Credentials,
	}
	if spec.Tier == fleet.TierBrowser {
		spec.Seccomp = browser.Seccomp
	}
	for _, m := range comp.Drives {
		spec.Drives = append(spec.Drives, driver.Mount{DriveID: driver.WorkDriveID(m.Name), At: m.At, ReadOnly: m.ReadOnly})
	}
	egress := slices.Sorted(slices.Values(comp.Egress.Allow))
	b, err := json.Marshal(struct {
		Image, Bundle string
		CPU, Memory   int64
		Drives        []driver.Mount
		Env           []string
		Egress        []string
		// Omitted when enforced, so enforced generations keep their hash.
		Unenforced bool `json:",omitempty"`
	}{spec.Image, bundle, spec.CPUMillis, spec.Memory, spec.Drives, spec.Env(c.cfg.Fleet), egress, f.Egress.Unenforced()})
	if err != nil {
		return spec, "", err
	}
	sum := sha256.Sum256(b)
	return spec, hex.EncodeToString(sum[:]), nil
}

// hashPresetTar is fleet.HashPreset computed over the tar stream
// fleet.WritePresetTar made of the same directory.
func hashPresetTar(data []byte) (string, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	h := sha256.New()
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("preset tar: %w", err)
		}
		rel := strings.TrimSuffix(hdr.Name, "/")
		switch hdr.Typeflag {
		case tar.TypeDir:
			fmt.Fprintf(h, "d %q %o\n", rel, hdr.Mode)
		case tar.TypeSymlink:
			fmt.Fprintf(h, "l %q %q\n", rel, hdr.Linkname)
		case tar.TypeReg:
			fh := sha256.New()
			if _, err := io.Copy(fh, tr); err != nil {
				return "", fmt.Errorf("preset tar: %w", err)
			}
			fmt.Fprintf(h, "f %q %o %d %s\n", rel, hdr.Mode, hdr.Size, hex.EncodeToString(fh.Sum(nil)))
		default:
			return "", fmt.Errorf("preset tar: unsupported entry %q", hdr.Name)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
