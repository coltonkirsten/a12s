package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
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
	o.whole, o.creds = true, req.Credentials
	return c.applySpec(ctx, func(*fleet.Spec) (*fleet.Spec, error) { return req.Fleet, nil }, o)
}

// writeOpts says what a write may touch and how it is admitted.
type writeOpts struct {
	whole bool // PUT /v1/fleet: may touch everything
	// only names the agent computers and Services a partial write touches.
	only    []string
	dryRun  bool
	ifMatch *int64
	by      caller
	creds   api.Credentials
}

func (o writeOpts) touches(name string) bool { return o.whole || slices.Contains(o.only, name) }

// applySpec runs mutate on a copy of the stored Spec under the exclusive apply lock, admits the result and converges
// what the write touches. Nothing changes when admission fails or on a dry run.
func (c *Controller) applySpec(ctx context.Context, mutate func(cur *fleet.Spec) (*fleet.Spec, error), o writeOpts) (api.PutFleetResponse, error) {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()

	c.mu.Lock()
	cur, version, oldFleet := c.state.Spec, c.state.Version, c.state.Fleet
	before := cur
	storedPins := c.state.Pins
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
	if err := checkScope(o.by, f, before, spec, o); err != nil {
		return api.PutFleetResponse{}, err
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
	changes := c.diff(oldFleet, f, o)
	// A dry run is admitted like the write, images and their manifests included, so it never says yes to a write
	// that fails.
	var admitWarnings []string
	pins, hosts, manifests, err := c.resolve(ctx, f, storedPins, &admitWarnings)
	if err != nil {
		return api.PutFleetResponse{}, err
	}
	admitSeccomp(f, &admitWarnings)
	waits := credentialWaits(f, creds, o)
	if o.dryRun {
		res := api.PutFleetResponse{Version: version, DryRun: true, Changes: changes, Created: []string{}, Updated: []string{}, Staged: []string{}, Removed: []string{},
			Warnings: slices.Concat(admitWarnings, f.InboxWarnings(), f.GrantWarnings()), Services: api.ServiceChanges{Created: []string{}, Updated: []string{}, Removed: []string{}}}
		if o.by.prefix() != "" {
			res.Warnings = nil
		}
		res.Warnings = append(res.Warnings, waits...)
		return res, nil
	}

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
	c.state.Spec, c.state.Fleet, c.state.Pins = spec, f, pins
	if changed {
		c.state.Version++
	}
	version = c.state.Version
	maps.Copy(c.state.BundleHosts, hosts)
	for _, name := range f.ServiceNames() {
		c.state.Manifests[name] = manifests[name]
		srec := c.state.Services[name]
		if srec == nil {
			srec = &ServiceRecord{}
			c.state.Services[name] = srec
		}
		if o.touches(name) && srec.Restarts != 0 {
			srec.Restarts = 0
			c.publishServicePhaseLocked(name)
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
			if sleptMarks[name] {
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

	res := api.PutFleetResponse{Version: version, Changes: changes, Created: []string{}, Updated: []string{}, Staged: []string{}, Removed: []string{}, Warnings: slices.Concat(admitWarnings, f.InboxWarnings(), f.GrantWarnings()),
		Services: api.ServiceChanges{Created: []string{}, Updated: []string{}, Removed: []string{}}}
	obs, err := c.drv.Observe(ctx)
	if err != nil {
		return res, err
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
		if errors.As(o.err, new(*credentialsMissing)) {
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
		if errors.As(o.err, new(*credentialsMissing)) {
			o.err = nil
		}
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
	if o.by.prefix() != "" {
		// Fleet-wide, so they may name what the token must not see.
		res.Warnings = nil
	}
	res.Warnings = append(res.Warnings, waits...)
	return res, nil
}

// credentialWaits names what this write touches that will wait for a credential with no value yet.
func credentialWaits(f *fleet.Fleet, creds api.Credentials, o writeOpts) []string {
	var out []string
	for _, name := range f.ComputerNames() {
		if missing := missingCredentials(f.Computers[name].Credentials, creds); o.touches(name) && len(missing) > 0 {
			out = append(out, "agent_computers."+name+": "+credentialWait(missing).String())
		}
	}
	for _, name := range f.ServiceNames() {
		if missing := missingCredentials(f.Services[name].Credentials, creds); o.touches(name) && len(missing) > 0 {
			out = append(out, "services."+name+": "+credentialWait(missing).String())
		}
	}
	return out
}

// admitFields refuses what only a Fleet file may hold, and names fields that neither the spec nor the defaults set.
func admitFields(f *fleet.Fleet, o writeOpts) error {
	var missing, fileOnly, relHosts []string
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
		if comp.Security.Seccomp.Path != "" {
			fileOnly = append(fileOnly, field+".security.seccomp")
		}
		for _, an := range slices.Sorted(maps.Keys(comp.Attach)) {
			if fleet.HostIsFileRelative(comp.Attach[an].Host) {
				relHosts = append(relHosts, field+".attach."+an+".host")
			}
		}
	}
	if len(fileOnly) > 0 {
		return &httpError{status: http.StatusBadRequest, body: api.Error{Code: api.ErrInvalid,
			Message: strings.Join(fileOnly, ", ") + ": a path exists only in a Fleet file; send the seccomp profile as JSON",
			Fix:     "a12s up does this; from the API set security.seccomp to the profile itself"}}
	}
	if len(relHosts) > 0 {
		return &httpError{status: http.StatusBadRequest, body: api.Error{Code: api.ErrInvalid,
			Message: strings.Join(relHosts, ", ") + ": a relative or ~ path exists only in a Fleet file",
			Fix:     "a12s up resolves it; from the API send the folder's absolute path"}}
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
	if exists {
		c.rtLocked(name).identity = obs.Identity
	}
	c.mu.Unlock()
	if _, err := c.drv.EnsureDrive(ctx, driver.DriveSpec{ID: spec.HomeDrive, Kind: driver.DriveHome, Computer: name}); err != nil {
		return unchanged, c.fail(name, err, true)
	}
	for _, a := range spec.Attach {
		for _, k := range a.Keep {
			if _, err := c.drv.EnsureDrive(ctx, driver.DriveSpec{ID: k.Drive, Kind: driver.DriveKeep, Computer: name, Path: k.Target}); err != nil {
				return unchanged, c.fail(name, err, true)
			}
		}
	}

	running := exists && obs.Power == driver.PowerAwake
	if exists && obs.Generation == rec.Generation && hash == rec.SpecHash {
		c.mu.Lock()
		if rec.StagedGeneration != 0 { // the Fleet went back to what runs
			rec.StagedGeneration, rec.StagedHash = 0, ""
		}
		// Same spec hash, so the same egress and bundle: this only
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
			return staged, c.stageLater(name, hash, obs.Generation, "applies when the current turn and background work end")
		}
	}
	return restaged, c.restage(ctx, name)
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
		Send:        slices.Clone(comp.Grants.Send),
		Uses:        c.endpointsLocked(comp.Uses),
		Seccomp:     comp.Security.Seccomp.Digest(),
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
// computerd that does not answer may be running one: unknown is busy. The cached phase is never trusted:
// a just-accepted turn's events may not have arrived.
func (c *Controller) busy(ctx context.Context, name string) bool {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := c.client(name).Status(sctx)
	if err != nil {
		return true
	}
	return st.ActiveTurn != "" || st.Phase == "busy" || st.Background != nil && st.Background.Tasks > 0
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
	var identity string
	err = c.confirm(ctx, name, fmt.Sprintf("generation %d staged", spec.Generation), func(st driver.ComputerState, exists bool) bool {
		identity = st.Identity
		return exists && st.Generation == spec.Generation && !st.Running
	})
	if err != nil {
		return c.fail(name, err, true)
	}
	c.mu.Lock()
	rec.Generation, rec.SpecHash = spec.Generation, hash
	rec.StagedGeneration, rec.StagedHash = 0, ""
	c.rtLocked(name).identity = identity
	// The new generation's egress policy takes effect now.
	rec.Active = c.activeLocked(name)
	desired := rec.DesiredPower
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	c.logf("%s: generation %d staged", name, spec.Generation)
	c.setTransition(name, false)
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
	c.closeAdmission(name)
	if !c.idle(c.ctx, name) {
		c.openAdmission(name)
		return
	}
	if err := c.restage(c.ctx, name); err != nil {
		c.openAdmission(name)
	}
	c.kick(name)
}

// SetPower is `a12s sleep` and `a12s wake`.
func (c *Controller) SetPower(ctx context.Context, name string, req api.PowerRequest) error {
	defer c.lockComputer(name)()
	if req.State == api.PowerAsleep && c.inbox != nil {
		// The intent is durable before the sleep starts: a crash in the
		// middle can't leave a held sleep unmarked.
		if err := c.inbox.SetSlept(ctx, name, sleptBy(req)); err != nil {
			return err
		}
	}
	if err := c.setPowerLocked(ctx, name, req); err != nil {
		return err
	}
	// A held sleep suppresses auto-wake until a manual wake, which clears the mark and a
	// paused auto-wake.
	if c.inbox != nil {
		by := ""
		if req.State == api.PowerAsleep {
			by = sleptBy(req)
		}
		if err := c.inbox.SetSlept(ctx, name, by); err != nil {
			c.logf("%s: power mark: %v", name, err)
		}
	}
	return nil
}

func sleptBy(req api.PowerRequest) string {
	if req.Hold {
		return api.SleptHeld
	}
	return api.SleptManual
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
		obs, ok, err := c.observeOne(ctx, name)
		if err != nil {
			return err
		}
		_, err = c.converge(ctx, name, obs, ok)
		var cm *credentialsMissing
		if errors.As(err, &cm) {
			return &httpError{status: http.StatusBadRequest, body: api.Error{Code: api.ErrInvalid, Message: name + ": " + err.Error(), Fix: cm.waiting.Fix}}
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
	if err := c.removeFromDriver(ctx, name); err != nil {
		c.setTransition(name, false)
		return err
	}
	c.mu.Lock()
	delete(c.state.Computers, name)
	delete(c.rt, name)
	delete(c.clients, name)
	err := c.saveLocked()
	c.mu.Unlock()
	if c.inbox != nil {
		if err := c.inbox.DeleteTurns(ctx, name); err != nil {
			c.logf("%s: turn history: %v", name, err)
		}
	}
	c.logf("%s: removed (drives kept)", name)
	return err
}

// resolve checks the Fleet against the substrate, makes the images and
// bundles available, and returns the pins with missing ones
// added (an existing pin never changes) and each bundle's sign-in hosts.
func (c *Controller) resolve(ctx context.Context, f *fleet.Fleet, in *fleet.Pins, warnings *[]string) (*fleet.Pins, map[string][]string, map[string]*fleet.ServiceManifest, error) {
	pins, hosts, caps, err := c.resolveComputers(ctx, f, in, warnings)
	if err != nil {
		return nil, nil, nil, err
	}
	manifests, err := c.resolveServices(ctx, f, pins, caps)
	if err != nil {
		return nil, nil, nil, err
	}
	return pins, hosts, manifests, nil
}

func (c *Controller) resolveComputers(ctx context.Context, f *fleet.Fleet, in *fleet.Pins, warnings *[]string) (*fleet.Pins, map[string][]string, driver.Capabilities, error) {
	invalid := func(format string, args ...any) error {
		return errorf(http.StatusBadRequest, api.ErrInvalid, format, args...)
	}
	caps, err := c.drv.Capabilities(ctx)
	if err != nil {
		return nil, nil, caps, err
	}
	if !caps.EgressEnforcement {
		return nil, nil, caps, invalid("this substrate cannot enforce egress  fix: upgrade Docker Engine to 28 or later")
	}
	if !caps.HostFolders {
		for _, name := range f.ComputerNames() {
			if len(f.Computers[name].Attach) > 0 {
				return nil, nil, caps, &httpError{status: http.StatusBadRequest, body: api.Error{Code: api.ErrInvalid,
					Message: fmt.Sprintf("agent_computers.%s.attach: this substrate has no host folders to attach", name),
					Fix:     "put the files in files:, or serve them from a Service"}}
			}
		}
	}

	pins := fleet.NewPins()
	if in != nil {
		maps.Copy(pins.Images, in.Images)
		maps.Copy(pins.Bundles, in.Bundles)
		maps.Copy(pins.Services, in.Services)
	}
	hosts := map[string][]string{}
	seen := map[string]bool{}
	for _, name := range f.ComputerNames() {
		comp := f.Computers[name]
		if !seen["image "+comp.Image] {
			seen["image "+comp.Image] = true
			id, err := ensurePinned(ctx, imageRef(comp.Image), pins.Images[imageRef(comp.Image)], c.drv.EnsureImage)
			if err != nil {
				return nil, nil, caps, fmt.Errorf("agent_computers.%s.image: %w", name, err)
			}
			if comp.Tier() == fleet.TierCustom {
				warn, err := c.admitComputerImage(ctx, comp.Image, id)
				if err != nil {
					return nil, nil, caps, invalid("agent_computers.%s.image: %v", name, err)
				}
				if warn != "" {
					*warnings = append(*warnings, fmt.Sprintf("agent_computers.%s.image: %s: %s", name, comp.Image, warn))
				}
			}
			pins.Images[imageRef(comp.Image)] = id
		}
		if key := comp.Harness.BundleKey(); hosts[key] == nil {
			var b driver.Bundle
			id, err := ensurePinned(ctx, bundleRef(comp.Harness), pins.Bundles[key], func(ctx context.Context, ref string) (string, error) {
				var err error
				b, err = c.drv.EnsureBundle(ctx, ref)
				return b.ImageID, err
			})
			if err != nil {
				return nil, nil, caps, fmt.Errorf("agent_computers.%s.harness: %w", name, err)
			}
			m := b.Manifest
			if m.Kind != comp.Harness.Kind || m.Version != comp.Harness.Version {
				return nil, nil, caps, invalid("agent_computers.%s.harness: bundle %s is %s@%s", name, id, m.Kind, m.Version)
			}
			if m.Arch != "" && !slices.Contains(caps.Architectures, m.Arch) {
				return nil, nil, caps, invalid("agent_computers.%s.harness: bundle is for %s, the substrate runs %s", name, m.Arch, strings.Join(caps.Architectures, ", "))
			}
			pins.Bundles[key] = id
			hosts[key] = append([]string{}, m.SignInHosts...)
		}
	}
	return pins, hosts, caps, nil
}

// ensurePinned makes ref available and returns its image ID: the pinned ID
// while it is available, so a running Fleet never moves under a moved tag.
func ensurePinned(ctx context.Context, ref, pin string, ensure func(context.Context, string) (string, error)) (string, error) {
	if pin == "" {
		return ensure(ctx, ref)
	}
	if id, err := ensure(ctx, pin); err == nil && id == pin {
		return id, nil
	}
	// The pinned image is gone (pruned or rebuilt under the same tag): the ref is what there is now.
	return ensure(ctx, ref)
}

// buildSpec is the Driver spec of a Computer under the applied Fleet and
// its pins (generation and files bytes unset), and the hash of what makes a
// generation: image, bundle, resources, environment, egress, attached folders and the files' and seccomp profile's
// digests.
func (c *Controller) buildSpec(name string) (driver.ComputerSpec, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, pins := c.state.Fleet, c.state.Pins
	if f == nil || pins == nil {
		return driver.ComputerSpec{}, "", errors.New("no Fleet applied")
	}
	comp, ok := f.Computers[name]
	if !ok {
		return driver.ComputerSpec{}, "", fmt.Errorf("no computer %q in the Fleet", name)
	}
	image, bundle := pins.Images[imageRef(comp.Image)], pins.Bundles[comp.Harness.BundleKey()]
	if image == "" || bundle == "" {
		return driver.ComputerSpec{}, "", errors.New("image or bundle is not pinned; run a12s up")
	}
	spec := driver.ComputerSpec{
		Name:        name,
		Image:       image,
		Tier:        comp.Tier(),
		Bundle:      driver.BundleRef{Kind: comp.Harness.Kind, Version: comp.Harness.Version, Image: bundle},
		Model:       comp.Harness.Model,
		Settings:    comp.Harness.Settings,
		CPUMillis:   comp.Resources.CPU.Millis(),
		Memory:      int64(comp.Resources.Memory),
		HomeDrive:   driver.HomeDriveID(name),
		Credentials: comp.Credentials,
		Uses:        c.endpointsLocked(comp.Uses),
		Send:        slices.Clone(comp.Grants.Send),
	}
	if files := comp.HomeFiles(); len(files) > 0 {
		var err error
		if spec.Files, spec.FilesDigest, err = fleet.FilesTar(files); err != nil {
			return driver.ComputerSpec{}, "", err
		}
	}
	seccompDigest := comp.Security.Seccomp.Digest()
	spec.Seccomp = string(comp.Security.Seccomp.Profile)
	spec.Attach = attachSpec(name, comp)
	egress := slices.Sorted(slices.Values(comp.Egress.Allow))
	b, err := json.Marshal(struct {
		Image, Bundle string
		CPU, Memory   int64
		Env           []string
		Egress        []string
		Files         string          `json:",omitempty"`
		Seccomp       string          `json:",omitempty"`
		Attach        []driver.Attach `json:",omitempty"`
	}{spec.Image, bundle, spec.CPUMillis, spec.Memory, spec.Env(c.cfg.Fleet), egress, spec.FilesDigest, seccompDigest, spec.Attach})
	if err != nil {
		return spec, "", err
	}
	sum := sha256.Sum256(b)
	return spec, hex.EncodeToString(sum[:]), nil
}

// attachSpec is the Driver's view of an agent computer's attached folders, sorted by name.
func attachSpec(name string, comp fleet.Computer) []driver.Attach {
	var out []driver.Attach
	for _, an := range slices.Sorted(maps.Keys(comp.Attach)) {
		a := comp.Attach[an]
		da := driver.Attach{Host: a.Host, Target: fleet.AttachTarget(an), ReadOnly: a.ReadOnly}
		for _, k := range a.Keep {
			da.Keep = append(da.Keep, driver.Keep{Drive: driver.KeepDriveID(name, an, k), Target: da.Target + "/" + k})
		}
		out = append(out, da)
	}
	return out
}

// keepDrives are the keep drive IDs the Fleet declares, each with its agent computer.
func keepDrives(f *fleet.Fleet) map[string]string {
	out := map[string]string{}
	for _, name := range f.ComputerNames() {
		for _, a := range attachSpec(name, f.Computers[name]) {
			for _, k := range a.Keep {
				out[k.Drive] = name
			}
		}
	}
	return out
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
