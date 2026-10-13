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

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/inbox"
	"github.com/coltonkirsten/a12s/internal/version"
)

func imageRef(image string) string {
	switch image {
	case fleet.TierSlim:
		return version.Image(image)
	}
	return image
}

func bundleRef(h fleet.Harness) string { return version.Bundle(h.Kind, h.Version) }

// PutFleet is `a12s up`: the whole Fleet. Admission errors change nothing.
func (c *Controller) PutFleet(ctx context.Context, req api.PutFleetRequest, o writeOpts) (api.PutFleetResponse, error) {
	if req.Fleet == nil {
		return api.PutFleetResponse{}, errorf(http.StatusBadRequest, api.ErrInvalid, "fleet is required")
	}
	o.whole, o.file, o.lock, o.creds, o.asleep, o.refresh = true, req.File, req.Lock, req.Credentials, req.Asleep, req.Refresh
	return c.applySpec(ctx, func(*fleet.Spec) (*fleet.Spec, error) { return req.Fleet, nil }, o)
}

// writeOpts says what a write may touch and how it is admitted.
type writeOpts struct {
	whole bool // PUT /v1/fleet: may touch everything
	file  bool // the Fleet comes from a Fleet file: it becomes the Applied spec
	// only names the agent computers and Services a partial write touches.
	only    []string
	dryRun  bool
	ifMatch *int64
	by      caller
	lock    *fleet.Lock // nil: the stored lock
	creds   api.Credentials
	asleep  bool
	refresh []string
}

func (o writeOpts) touches(name string) bool { return o.whole || slices.Contains(o.only, name) }

// applySpec runs mutate on a copy of the stored Spec under the exclusive apply lock, admits the result and converges
// what the write touches. Nothing changes when admission fails or on a dry run.
func (c *Controller) applySpec(ctx context.Context, mutate func(cur *fleet.Spec) (*fleet.Spec, error), o writeOpts) (api.PutFleetResponse, error) {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()

	c.mu.Lock()
	cur, version, oldFleet := c.state.Spec, c.state.Version, c.state.Fleet
	storedLock := c.state.Lock
	c.mu.Unlock()
	if o.ifMatch != nil && *o.ifMatch != version {
		return api.PutFleetResponse{}, &httpError{status: http.StatusPreconditionFailed, body: api.Error{Code: api.ErrVersionMismatch,
			Message: fmt.Sprintf("the Fleet is at version %d, not %d: it changed since you read it", version, *o.ifMatch),
			Fix:     "read it again (GET /v1/fleet) and retry; the SDK's fleet.replace does this for you"}}
	}
	if cur == nil {
		cur = &fleet.Spec{APIVersion: fleet.APIVersion, Kind: fleet.Kind, Metadata: fleet.Metadata{Name: c.cfg.Fleet}, AgentComputers: map[string]json.RawMessage{}}
	} else {
		cur = cur.Clone()
	}
	spec, err := mutate(cur)
	if err != nil {
		return api.PutFleetResponse{}, err
	}
	canonicalize(spec)
	if spec.Metadata.Name != c.cfg.Fleet {
		return api.PutFleetResponse{}, errorf(http.StatusBadRequest, api.ErrInvalid, "this Controller runs fleet %q, not %q", c.cfg.Fleet, spec.Metadata.Name)
	}
	f, err := spec.Effective()
	if err != nil {
		return api.PutFleetResponse{}, errorf(http.StatusBadRequest, api.ErrInvalid, "%v", err)
	}
	if err := admitFields(f, o); err != nil {
		return api.PutFleetResponse{}, err
	}
	if err := f.Validate(); err != nil {
		return api.PutFleetResponse{}, errorf(http.StatusBadRequest, api.ErrInvalid, "%v", err)
	}
	c.mu.Lock()
	creds := maps.Clone(c.creds)
	c.mu.Unlock()
	maps.Copy(creds, o.creds)
	for _, name := range f.ComputerNames() {
		if !o.touches(name) {
			continue
		}
		if missing := missingCredentials(f.Computers[name].Credentials, creds); len(missing) > 0 {
			return api.PutFleetResponse{}, &httpError{status: http.StatusBadRequest, body: api.Error{Code: api.ErrMissingCredential,
				Message: fmt.Sprintf("agent_computers.%s: credentials %s have no value", name, strings.Join(missing, ", ")),
				Fix:     "fleet.credentials.set(name, value), or a12s creds set <name> and a12s up"}}
		}
	}
	changes := c.diff(oldFleet, f, o)
	if o.dryRun {
		return api.PutFleetResponse{Version: version, DryRun: true, Changes: changes, Created: []string{}, Updated: []string{}, Staged: []string{}, Removed: []string{},
			Warnings: slices.Concat(f.InboxWarnings(), f.GrantWarnings()), Services: api.ServiceChanges{Created: []string{}, Updated: []string{}, Removed: []string{}}}, nil
	}
	lockIn := o.lock
	if lockIn == nil {
		lockIn = storedLock
	}
	var admitWarnings []string
	lock, hosts, manifests, err := c.resolve(ctx, f, lockIn, o.refresh, &admitWarnings)
	if err != nil {
		return api.PutFleetResponse{}, err
	}
	if err := serviceCredentialErrors(f, manifests, creds); err != nil {
		return api.PutFleetResponse{}, err
	}
	admitSeccomp(f, &admitWarnings)

	// Durable sleep marks, read before taking the lock.
	sleptMarks := map[string]bool{}
	if c.inbox != nil {
		for _, name := range f.ComputerNames() {
			c.mu.Lock()
			known := c.state.Computers[name] != nil
			c.mu.Unlock()
			if known {
				continue
			}
			if row, err := c.inbox.Power(ctx, name); err == nil && row.SleptBy != "" {
				sleptMarks[name] = true
			}
		}
	}
	// Admitted: this Fleet is now the desired state.
	c.mu.Lock()
	var previous map[string]fleet.Computer
	var previousServices map[string]fleet.Service
	if c.state.Fleet != nil {
		previous, previousServices = c.state.Fleet.Computers, c.state.Fleet.Services
	}
	changed := c.state.Spec == nil || specJSON(c.state.Spec) != specJSON(spec)
	c.state.Spec, c.state.Fleet, c.state.Lock = spec, f, lock
	if changed {
		c.state.Version++
	}
	if o.file {
		c.state.Applied = spec.Clone()
	}
	version = c.state.Version
	maps.Copy(c.state.BundleHosts, hosts)
	newServices := []string{}
	for _, name := range f.ServiceNames() {
		c.state.Manifests[name] = manifests[name]
		srec := c.state.Services[name]
		if srec == nil {
			srec = &ServiceRecord{}
			c.state.Services[name] = srec
			newServices = append(newServices, name)
		}
		if o.touches(name) {
			srec.Restarts = 0
		}
	}
	if len(o.creds) > 0 {
		if err := c.setCredsLocked(o.creds); err != nil {
			c.mu.Unlock()
			return api.PutFleetResponse{}, err
		}
	}
	for _, name := range f.ComputerNames() {
		rec := c.state.Computers[name]
		if rec == nil {
			rec = &Record{DesiredPower: driver.PowerAwake}
			if o.asleep || sleptMarks[name] {
				// A Computer that went to sleep before `down` comes back asleep: the mark is Fleet data
				// in fleet.db.
				rec.DesiredPower = driver.PowerAsleep
			}
			c.state.Computers[name] = rec
		}
		if o.touches(name) {
			rec.Restarts = 0
		}
	}
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return api.PutFleetResponse{}, err
	}
	if c.inbox != nil {
		for _, name := range newServices {
			if _, ok, err := c.inbox.ServiceDesired(ctx, name); err == nil && !ok {
				desired := driver.PowerAwake
				if o.asleep {
					desired = driver.PowerAsleep
				}
				_ = c.inbox.SetServiceDesired(ctx, name, string(desired))
			}
		}
	}
	if err := c.syncInboxes(ctx, f, api.PrincipalLocal); err != nil {
		return api.PutFleetResponse{}, err
	}
	c.syncPower(ctx, f, api.PrincipalLocal)
	if c.inbox != nil && o.whole {
		_ = c.inbox.ClearPaused(ctx) // a12s up resumes an auto-wake paused by a failed wake
	}
	if changed {
		c.fleetChanged(version, o.by.principal, changes)
	}

	res := api.PutFleetResponse{Version: version, Changes: changes, Lock: lock, Created: []string{}, Updated: []string{}, Staged: []string{}, Removed: []string{}, Warnings: slices.Concat(admitWarnings, f.InboxWarnings(), f.GrantWarnings()),
		Services: api.ServiceChanges{Created: []string{}, Updated: []string{}, Removed: []string{}}}
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

	// Services first: nothing about a Computer waits on them, but a
	// Computer that uses one should find it running.
	goneSvc := map[string]bool{}
	for name := range previousServices {
		goneSvc[name] = true
	}
	for name := range obs.Services {
		goneSvc[name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(goneSvc)) {
		if _, ok := f.Services[name]; ok || !o.touches(name) {
			continue
		}
		if err := c.removeService(ctx, name); err != nil {
			return res, err
		}
		res.Services.Removed = append(res.Services.Removed, name)
	}
	svcNames := slices.DeleteFunc(f.ServiceNames(), func(n string) bool { return !o.touches(n) })
	souts := c.convergeServices(ctx, svcNames, obs)
	for _, name := range svcNames {
		o := souts[name]
		_, existed := previousServices[name]
		var waiting *serviceWaiting
		if errors.As(o.err, &waiting) {
			res.Warnings = append(res.Warnings, name+": "+waiting.Error())
			o.err = nil
		}
		switch {
		case o.err != nil:
			if res.Errors == nil {
				res.Errors = map[string]string{}
			}
			res.Errors[name] = o.err.Error()
			if !existed {
				res.Services.Created = append(res.Services.Created, name)
			}
		case !existed:
			res.Services.Created = append(res.Services.Created, name)
		case o.result == restaged:
			res.Services.Updated = append(res.Services.Updated, name)
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
		if _, ok := f.Computers[name]; ok || !o.touches(name) {
			continue
		}
		if err := c.removeComputer(ctx, name); err != nil {
			return res, err
		}
		res.Removed = append(res.Removed, name)
	}

	names := slices.DeleteFunc(f.ComputerNames(), func(n string) bool { return !o.touches(n) })
	outcomes := c.convergeAll(ctx, names, obs)
	for _, name := range names {
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
	for i, ch := range res.Changes {
		if ch.Kind == api.KindAgentComputer && ch.Op == api.ChangeUpdate {
			res.Changes[i].When = api.WhenNow
			if slices.Contains(res.Staged, ch.Name) {
				res.Changes[i].When = api.WhenIdle
			}
		}
	}
	c.refreshAddrs(ctx)
	return res, nil
}

// admitFields refuses what only a Fleet file may hold, and names fields that neither the spec nor the defaults set.
func admitFields(f *fleet.Fleet, o writeOpts) error {
	var missing, fileOnly []string
	for _, name := range f.ComputerNames() {
		comp := f.Computers[name]
		field := "agent_computers." + name
		if comp.Harness.Kind == "" {
			missing = append(missing, field+".harness.kind")
		}
		if comp.Harness.Version == "" {
			missing = append(missing, field+".harness.version")
		}
		if comp.Image == "" {
			missing = append(missing, field+".image")
		}
		if comp.Home.Preset != "" {
			fileOnly = append(fileOnly, field+".home.preset")
		}
		if comp.Security.Seccomp.Path != "" {
			fileOnly = append(fileOnly, field+".security.seccomp")
		}
	}
	if len(fileOnly) > 0 {
		return &httpError{status: http.StatusBadRequest, body: api.Error{Code: api.ErrInvalid,
			Message: strings.Join(fileOnly, ", ") + ": paths exist only in a Fleet file; send the preset as files and the seccomp profile as JSON",
			Fix:     "a12s up does this; from the API set instructions, files and security.seccomp inline"}}
	}
	if len(missing) > 0 {
		return &httpError{status: http.StatusBadRequest, body: api.Error{Code: api.ErrMissingField,
			Message: strings.Join(missing, ", ") + ": not set here or in defaults",
			Fix:     "set it on the agent computer or in defaults"}}
	}
	return nil
}

// canonicalize re-encodes every entry so equal specs are equal JSON.
func canonicalize(s *fleet.Spec) {
	if s.AgentComputers == nil {
		s.AgentComputers = map[string]json.RawMessage{}
	}
	for n, raw := range s.AgentComputers {
		s.AgentComputers[n] = json.RawMessage(fleet.Canonical(raw))
	}
	if len(s.Defaults) > 0 {
		if d := fleet.Canonical(s.Defaults); d == "{}" {
			s.Defaults = nil
		} else {
			s.Defaults = json.RawMessage(d)
		}
	}
}

func specJSON(s *fleet.Spec) string {
	b, _ := json.Marshal(s)
	return string(b)
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

func (c *Controller) activeLocked(name string) Active {
	comp := c.state.Fleet.Computers[name]
	return Active{
		Allow:       slices.Clone(comp.Egress.Allow),
		SignInHosts: slices.Clone(c.state.BundleHosts[comp.Harness.BundleKey()]),
		Drives:      slices.Clone(comp.Drives),
		Unenforced:  c.state.Fleet.Egress.Unenforced(),
		Send:        slices.Clone(comp.Grants.Send),
		Uses:        c.endpointsLocked(comp.Uses),
		Seccomp:     comp.Security.Seccomp.Digest(),
	}
}

// driveHeld means an exclusive drive the new generation attaches is still
// attached to another Computer (whose own change waits for its idle
// boundary). It is a dependency, not a failure.
type driveHeld struct{ drive, holder string }

func (e *driveHeld) Error() string {
	return "waits for " + e.holder + " to release exclusive drive " + e.drive
}

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
		if err := c.inject(ctx, name, false); err != nil {
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
	if req.State == api.PowerAsleep && c.inbox != nil {
		// The intent is durable before the sleep starts: a crash in the
		// middle can't leave a manual sleep unmarked.
		by := api.SleptManual
		if req.AllowWake {
			by = api.SleptAllowWake
		}
		if err := c.inbox.SetSlept(ctx, name, by); err != nil {
			return err
		}
	}
	if err := c.setPowerLocked(ctx, name, req); err != nil {
		return err
	}
	// A manual sleep suppresses auto-wake until a manual wake, which clears the mark and a
	// paused auto-wake.
	if c.inbox != nil {
		by := ""
		if req.State == api.PowerAsleep {
			by = api.SleptManual
			if req.AllowWake {
				by = api.SleptAllowWake
			}
		}
		if err := c.inbox.SetSlept(ctx, name, by); err != nil {
			c.logf("%s: power mark: %v", name, err)
		}
	}
	return nil
}

func (c *Controller) setPowerLocked(ctx context.Context, name string, req api.PowerRequest) error {
	c.mu.Lock()
	rec := c.state.Computers[name]
	inFleet := c.state.Fleet != nil && rec != nil
	if inFleet {
		_, inFleet = c.state.Fleet.Computers[name]
	}
	if inFleet && req.State == api.PowerAwake {
		_ = c.setCredsLocked(req.Credentials)
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
		if req.ClearInbox && c.inbox != nil {
			// Recorded and applied before admission opens.
			id := req.RequestID
			if id == "" {
				id = "wake-" + inbox.NewULID(time.Now())
			}
			through, n, err := c.inbox.WakeClear(ctx, name, id, api.PrincipalLocal)
			if err != nil {
				return err
			}
			c.logf("%s: wake --clear-inbox discarded %d pending messages (through seq %d)", name, n, through)
			c.mu.Lock()
			c.rtLocked(name).lastClear = n
			c.mu.Unlock()
		}
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
func (c *Controller) resolve(ctx context.Context, f *fleet.Fleet, in *fleet.Lock, refresh []string, warnings *[]string) (*fleet.Lock, map[string][]string, map[string]*fleet.ServiceManifest, error) {
	lock, hosts, caps, err := c.resolveComputers(ctx, f, in, warnings)
	if err != nil {
		return nil, nil, nil, err
	}
	manifests, err := c.resolveServices(ctx, f, lock, caps, refresh)
	if err != nil {
		return nil, nil, nil, err
	}
	return lock, hosts, manifests, nil
}

func (c *Controller) resolveComputers(ctx context.Context, f *fleet.Fleet, in *fleet.Lock, warnings *[]string) (*fleet.Lock, map[string][]string, driver.Capabilities, error) {
	invalid := func(format string, args ...any) error {
		return errorf(http.StatusBadRequest, api.ErrInvalid, format, args...)
	}
	caps, err := c.drv.Capabilities(ctx)
	if err != nil {
		return nil, nil, caps, err
	}
	if !caps.EgressEnforcement && !f.Egress.Unenforced() {
		return nil, nil, caps, invalid("this substrate cannot enforce egress; set egress.enforcement: unenforced to run anyway")
	}
	for _, d := range f.DriveNames() {
		if f.Drives[d].Access == fleet.AccessSharedWrite && !caps.SharedWriteDrives {
			return nil, nil, caps, invalid("drives.%s: this substrate has no shared-write storage", d)
		}
	}

	lock := fleet.NewLock()
	if in != nil {
		maps.Copy(lock.Images, in.Images)
		maps.Copy(lock.Bundles, in.Bundles)
		maps.Copy(lock.Services, in.Services)
	}
	hosts := map[string][]string{}
	seen := map[string]bool{}
	for _, name := range f.ComputerNames() {
		comp := f.Computers[name]
		if !seen["image "+comp.Image] {
			seen["image "+comp.Image] = true
			id, err := ensurePinned(ctx, imageRef(comp.Image), lock.Images[comp.Image], c.drv.EnsureImage)
			if err != nil {
				return nil, nil, caps, fmt.Errorf("computers.%s.image: %w", name, err)
			}
			if comp.Tier() == fleet.TierCustom {
				warn, err := c.admitComputerImage(ctx, comp.Image, id)
				if err != nil {
					return nil, nil, caps, invalid("computers.%s.image: %v", name, err)
				}
				if warn != "" {
					*warnings = append(*warnings, fmt.Sprintf("computers.%s.image: %s: %s", name, comp.Image, warn))
				}
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
				return nil, nil, caps, fmt.Errorf("computers.%s.harness: %w", name, err)
			}
			m := b.Manifest
			if m.Kind != comp.Harness.Kind || m.Version != comp.Harness.Version {
				return nil, nil, caps, invalid("computers.%s.harness: bundle %s is %s@%s", name, id, m.Kind, m.Version)
			}
			if m.Arch != "" && !slices.Contains(caps.Architectures, m.Arch) {
				return nil, nil, caps, invalid("computers.%s.harness: bundle is for %s, the substrate runs %s", name, m.Arch, strings.Join(caps.Architectures, ", "))
			}
			lock.PinBundle(m.Kind, m.Version, id)
			hosts[key] = append([]string{}, m.SignInHosts...)
		}
	}
	return lock, hosts, caps, nil
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
// Lock (generation and preset bytes unset), and the hash of what makes a
// generation: image, bundle, resources, drives, environment, egress, the
// Fleet's egress enforcement and the preset's and seccomp profile's digests.
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
		Uses:        c.endpointsLocked(comp.Uses),
		Send:        slices.Clone(comp.Grants.Send),
	}
	if files := comp.HomeFiles(); len(files) > 0 {
		var err error
		if spec.Preset, spec.PresetDigest, err = fleet.FilesTar(files); err != nil {
			return driver.ComputerSpec{}, "", err
		}
	}
	seccompDigest := comp.Security.Seccomp.Digest()
	spec.Seccomp = string(comp.Security.Seccomp.Profile)
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
		Unenforced bool   `json:",omitempty"`
		Preset     string `json:",omitempty"`
		Seccomp    string `json:",omitempty"`
	}{spec.Image, bundle, spec.CPUMillis, spec.Memory, spec.Drives, spec.Env(c.cfg.Fleet), egress, f.Egress.Unenforced(), spec.PresetDigest, seccompDigest})
	if err != nil {
		return spec, "", err
	}
	sum := sha256.Sum256(b)
	return spec, hex.EncodeToString(sum[:]), nil
}

// admitSeccomp warns about profiles that allow every syscall they do not list. The profile comes only from the
// Fleet's owner, never from an image, and replaces nothing but the seccomp filter: capabilities and
// no-new-privileges stay as they are.
func admitSeccomp(f *fleet.Fleet, warnings *[]string) {
	for _, name := range f.ComputerNames() {
		p := f.Computers[name].Security.Seccomp.Profile
		if len(p) == 0 {
			continue
		}
		if allowAll, err := fleet.ValidateSeccomp(p); err == nil && allowAll {
			*warnings = append(*warnings, fmt.Sprintf("agent_computers.%s.security.seccomp: the profile allows every syscall it does not list", name))
		}
	}
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

// admitComputerImage accepts a custom image only if it was built FROM a compatible a12s base image: computerd is
// the Computer's whole contract with the Controller, and the inherited label says which release it came from.
func (c *Controller) admitComputerImage(ctx context.Context, ref, id string) (warning string, err error) {
	img, err := c.drv.InspectComputerImage(ctx, id)
	if err != nil {
		return "", err
	}
	base := version.Image(fleet.TierSlim)
	v := img.Labels[driver.LabelComputerdVersion]
	switch {
	case v == "":
		return "", fmt.Errorf("%s has no %s label, so it was not built FROM an a12s image  fix: start its Dockerfile with FROM %s", ref, driver.LabelComputerdVersion, base)
	case !img.Computerd:
		return "", fmt.Errorf("%s has no computerd at %s  fix: build it FROM %s and keep /opt/a12s", ref, driver.ComputerdMain, base)
	}
	warning, err = version.CheckComputerd(v)
	if err != nil {
		return "", fmt.Errorf("%s: %v  fix: rebuild it FROM %s", ref, err, base)
	}
	return warning, nil
}
