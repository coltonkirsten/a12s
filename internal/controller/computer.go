package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/computerd"
	"github.com/coltonkirsten/a12s/internal/driver"
)

// runtime is a Computer's in-memory state.
type runtime struct {
	phase         api.Phase
	failure       *api.Failure
	activeTurn    string
	resumePending bool
	// transition is set while the Controller itself stops, stages or
	// removes the Computer: deaths seen then are expected.
	transition   bool
	cancelEvents context.CancelFunc
	lastEventID  string
	// credsHash identifies the credential material last injected.
	credsHash string
	// admit is held from a prompt's admission check until computerd has
	// accepted it; closed (guarded by Controller.mu) stops admission while
	// the Controller stops or restages the Computer.
	admit  sync.Mutex
	closed bool
	// released is set while the old generation was removed to free its
	// drives and the new one is not staged yet.
	released bool
}

// closeAdmission stops new prompts and waits for one being admitted:
// afterwards computerd's status shows every accepted turn.
func (c *Controller) closeAdmission(name string) {
	c.mu.Lock()
	r := c.rtLocked(name)
	c.mu.Unlock()
	r.admit.Lock()
	c.mu.Lock()
	r.closed = true
	c.mu.Unlock()
	r.admit.Unlock()
}

func (c *Controller) openAdmission(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rtLocked(name).closed = false
}

// confirm reads the Computer back through Observe until want holds: a
// Driver call returning is not proof that it took effect (§4).
func (c *Controller) confirm(ctx context.Context, name, what string, want func(st driver.ComputerState, exists bool) bool) error {
	deadline := time.Now().Add(c.timing.ReadBack)
	for {
		st, exists, err := c.observeOne(ctx, name)
		if err == nil && want(st, exists) {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("observed generation %d, power %s, exists %v", st.Generation, st.Power, exists)
			}
			return fmt.Errorf("%s: %s not observed: %w", name, what, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.timing.Poll):
		}
	}
}

func (c *Controller) rtLocked(name string) *runtime {
	r := c.rt[name]
	if r == nil {
		r = &runtime{phase: api.PhaseProvisioning}
		c.rt[name] = r
	}
	return r
}

// setPhase records a phase change and logs it, one line per change.
func (c *Controller) setPhase(name string, phase api.Phase, failure *api.Failure) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setPhaseLocked(name, phase, failure)
}

func (c *Controller) setPhaseLocked(name string, phase api.Phase, failure *api.Failure) {
	r := c.rtLocked(name)
	if r.phase == phase && (failure == nil) == (r.failure == nil) && (failure == nil || *failure == *r.failure) {
		return
	}
	switch {
	case failure != nil:
		c.logf("%s: %s -> Error: %s (retryable: %v)", name, r.phase, failure.Reason, failure.Retryable)
	default:
		c.logf("%s: %s -> %s", name, r.phase, phase)
	}
	r.phase, r.failure = phase, failure
	if phase != api.PhaseBusy {
		r.activeTurn = ""
	}
}

// fail puts the Computer in Error and returns err.
func (c *Controller) fail(name string, err error, retryable bool) error {
	c.setPhase(name, api.PhaseError, &api.Failure{Reason: err.Error(), Retryable: retryable})
	return err
}

func (c *Controller) phase(name string) api.Phase {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rtLocked(name).phase
}

func (c *Controller) setTransition(name string, on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rtLocked(name).transition = on
}

// credsHashLocked identifies the material for the Computer's credentials,
// or "" when some is missing.
func (c *Controller) credsHashLocked(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, n := range sorted {
		v := c.creds[n]
		if len(v) == 0 {
			return ""
		}
		fmt.Fprintf(h, "%s %d\n", n, len(v))
		h.Write(v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// observeOne returns the Computer as the Driver sees it now.
func (c *Controller) observeOne(ctx context.Context, name string) (driver.ComputerState, bool, error) {
	obs, err := c.drv.Observe(ctx)
	if err != nil {
		return driver.ComputerState{}, false, err
	}
	st, ok := obs.Computers[name]
	return st, ok, nil
}

// start powers on the staged generation, injects credentials and a
// certificate, and waits for computerd: Starting -> Ready.
func (c *Controller) start(ctx context.Context, name string) error {
	c.setPhase(name, api.PhaseStarting, nil)
	c.mu.Lock()
	comp := c.state.Fleet.Computers[name]
	gen := c.state.Computers[name].Generation
	missing := missingCredentials(comp.Credentials, c.creds)
	c.mu.Unlock()
	if len(missing) > 0 {
		return c.fail(name, &credentialsMissing{names: missing}, true)
	}
	if err := c.verifyAddr(ctx, name); err != nil {
		return c.fail(name, err, true)
	}
	if err := c.drv.SetPower(ctx, name, driver.PowerAwake, gen); err != nil {
		return c.fail(name, err, true)
	}
	c.client(name).CloseIdleConnections()
	if err := c.inject(ctx, name); err != nil {
		return c.fail(name, err, true)
	}
	c.mu.Lock()
	c.rtLocked(name).credsHash = c.credsHashLocked(comp.Credentials)
	c.mu.Unlock()
	st, err := c.waitReady(ctx, name)
	if err == nil {
		err = c.confirmRunning(ctx, name, gen)
	}
	if err != nil {
		return c.fail(name, err, true)
	}
	c.observeStatus(name, st)
	c.openAdmission(name)
	c.subscribe(name)
	return nil
}

// confirmRunning reads back through Observe that the generation runs,
// before the Computer is published Ready.
func (c *Controller) confirmRunning(ctx context.Context, name string, gen int64) error {
	return c.confirm(ctx, name, fmt.Sprintf("generation %d running", gen), func(st driver.ComputerState, exists bool) bool {
		return exists && st.Generation == gen && st.Power == driver.PowerAwake && st.Running
	})
}

// waitReady polls computerd until it answers with admission open.
func (c *Controller) waitReady(ctx context.Context, name string) (api.ComputerdStatus, error) {
	deadline := time.Now().Add(c.timing.ReadyTimeout)
	cl := c.client(name)
	var last error
	for {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := cl.Status(sctx)
		cancel()
		if err == nil && st.Phase != "starting" {
			return st, nil
		}
		if err == nil {
			err = errors.New("computerd is starting")
		} else if obs, ok, oerr := c.observeOne(ctx, name); oerr == nil && (!ok || !obs.Running) {
			reason := "container is not running"
			if ok && obs.Failure != "" {
				reason = "container " + obs.Failure
			}
			return st, errors.New(reason)
		}
		last = err
		if time.Now().After(deadline) {
			return st, fmt.Errorf("computerd did not become ready within %s: %v", c.timing.ReadyTimeout, last)
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(c.timing.Poll):
		}
	}
}

// recordStatus keeps computerd's session and resumePending, persisted so an
// asleep Computer shows them.
func (c *Controller) recordStatus(name string, st api.ComputerdStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordStatusLocked(name, st)
}

func (c *Controller) recordStatusLocked(name string, st api.ComputerdStatus) {
	r := c.rtLocked(name)
	r.resumePending = st.ResumePending
	rec := c.state.Computers[name]
	if rec == nil {
		return
	}
	changed := rec.ResumePending != st.ResumePending
	rec.ResumePending = st.ResumePending
	if st.Session != "" && rec.Session != st.Session {
		rec.Session, changed = st.Session, true
	}
	if changed {
		_ = c.saveLocked()
	}
}

// finalStatus records computerd's last status before the Controller stops
// the Computer.
func (c *Controller) finalStatus(ctx context.Context, name string) {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if st, err := c.client(name).Status(sctx); err == nil {
		c.recordStatus(name, st)
	}
}

// observeStatus takes phase, active turn, session and resumePending from
// computerd's status.
func (c *Controller) observeStatus(name string, st api.ComputerdStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordStatusLocked(name, st)
	r := c.rtLocked(name)
	if r.transition {
		return
	}
	phase := api.PhaseReady
	switch {
	case st.Phase == "busy" || st.ActiveTurn != "":
		phase = api.PhaseBusy
	case st.Phase == "starting":
		phase = api.PhaseStarting
	case st.Phase == "stopping":
		phase = api.PhaseStopping
	}
	c.setPhaseLocked(name, phase, nil)
	r.activeTurn = st.ActiveTurn
}

// subscribe follows the Computer's event stream to track Ready ⇄ Busy.
func (c *Controller) subscribe(name string) {
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	r := c.rtLocked(name)
	if r.cancelEvents != nil {
		r.cancelEvents()
	}
	r.cancelEvents = cancel
	c.mu.Unlock()
	go c.followEvents(ctx, name)
}

func (c *Controller) stopEvents(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.rt[name]; r != nil && r.cancelEvents != nil {
		r.cancelEvents()
		r.cancelEvents = nil
	}
}

func (c *Controller) followEvents(ctx context.Context, name string) {
	cl := c.client(name)
	for ctx.Err() == nil {
		c.mu.Lock()
		last := c.rtLocked(name).lastEventID
		c.mu.Unlock()
		s, err := cl.Events(ctx, last, "")
		if err == nil {
			// The stream is open, so nothing after this status is missed.
			if st, err := cl.Status(ctx); err == nil {
				c.observeStatus(name, st)
				c.maybeApplyStaged(name)
			}
			c.readEvents(name, s)
			s.Close()
		}
		select {
		case <-ctx.Done():
		case <-time.After(c.timing.Poll):
		}
	}
}

func (c *Controller) readEvents(name string, s *computerd.Stream) {
	for {
		m, err := s.Next()
		if err != nil {
			return
		}
		if m.Event != "" {
			continue
		}
		ev, err := m.Decode()
		if err != nil || m.ID == "" || ev.Seq == 0 || ev.Type == "" {
			// Never skip a malformed event: end this stream, and the next
			// one starts again from the last good cursor.
			return
		}
		// The cursor moves only past an event that decoded, so a damaged
		// one is fetched again on the next connection instead of skipped.
		if m.ID != "" {
			c.mu.Lock()
			c.rtLocked(name).lastEventID = m.ID
			c.mu.Unlock()
		}
		switch ev.Type {
		case api.EventTurnStarted:
			c.mu.Lock()
			if r := c.rtLocked(name); !r.transition {
				c.setPhaseLocked(name, api.PhaseBusy, nil)
				r.activeTurn = ev.Turn
			}
			if rec := c.state.Computers[name]; rec != nil && ev.Session != "" && rec.Session != ev.Session {
				rec.Session = ev.Session
				_ = c.saveLocked()
			}
			c.mu.Unlock()
		case api.EventTurnFinished:
			c.mu.Lock()
			if r := c.rtLocked(name); !r.transition && r.phase == api.PhaseBusy {
				c.setPhaseLocked(name, api.PhaseReady, nil)
			}
			c.mu.Unlock()
			c.maybeApplyStaged(name)
		case api.EventSessionChanged:
			var d struct {
				To string `json:"to"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.To != "" {
				c.mu.Lock()
				if rec := c.state.Computers[name]; rec != nil && rec.Session != d.To {
					rec.Session = d.To
					_ = c.saveLocked()
				}
				c.mu.Unlock()
			}
		case api.EventResumeFailed:
			c.mu.Lock()
			c.rtLocked(name).resumePending = true
			if rec := c.state.Computers[name]; rec != nil && !rec.ResumePending {
				rec.ResumePending = true
				_ = c.saveLocked()
			}
			c.mu.Unlock()
		case api.EventLifecycle:
			sctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
			if st, err := c.client(name).Status(sctx); err == nil {
				c.recordStatus(name, st)
			}
			cancel()
		}
	}
}

// maybeApplyStaged applies a staged generation once the Computer is idle.
func (c *Controller) maybeApplyStaged(name string) {
	c.mu.Lock()
	rec := c.state.Computers[name]
	r := c.rtLocked(name)
	due := rec != nil && rec.pending() && r.phase == api.PhaseReady && !r.transition
	c.mu.Unlock()
	if due {
		go c.applyStaged(name)
	}
}

// attach picks up a Computer that is already running (after a Controller
// restart): it reads computerd's status and follows its events. Credentials
// already injected stay in the Computer's tmpfs.
func (c *Controller) attach(ctx context.Context, name string) error {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := c.client(name).Status(sctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	gen := c.state.Computers[name].Generation
	c.mu.Unlock()
	if err := c.confirmRunning(ctx, name, gen); err != nil {
		return err
	}
	c.observeStatus(name, st)
	c.openAdmission(name)
	c.subscribe(name)
	return nil
}

// sleep closes admission, interrupts the active turn, allows a bounded
// flush, and stops the Computer: Stopping -> Asleep (§7).
func (c *Controller) sleep(ctx context.Context, name string) error {
	c.mu.Lock()
	rec := c.state.Computers[name]
	rec.DesiredPower = driver.PowerAsleep
	err := c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	c.closeAdmission(name)
	obs, ok, err := c.observeOne(ctx, name)
	if err != nil {
		return err
	}
	if !ok || obs.Power != driver.PowerAwake {
		c.stopEvents(name)
		c.setPhase(name, api.PhaseAsleep, nil)
		return nil
	}
	c.setPhase(name, api.PhaseStopping, nil)
	c.setTransition(name, true)
	defer c.setTransition(name, false)
	c.interrupt(ctx, name, c.timing.SleepFlush)
	c.finalStatus(ctx, name)
	c.stopEvents(name)
	if err := c.stop(ctx, name, obs.Generation); err != nil {
		return c.fail(name, err, true)
	}
	c.setPhase(name, api.PhaseAsleep, nil)
	return nil
}

// stop powers the Computer down and reads that back.
func (c *Controller) stop(ctx context.Context, name string, gen int64) error {
	if err := c.drv.SetPower(ctx, name, driver.PowerAsleep, gen); err != nil {
		return err
	}
	return c.confirm(ctx, name, "power asleep", func(st driver.ComputerState, exists bool) bool {
		return !exists || st.Power == driver.PowerAsleep
	})
}

// interrupt stops the active turn, if any, and waits up to grace for it
// to finish. It reports whether the Computer is idle.
func (c *Controller) interrupt(ctx context.Context, name string, grace time.Duration) bool {
	cl := c.client(name)
	deadline := time.Now().Add(grace)
	stopped := ""
	for {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := cl.Status(sctx)
		cancel()
		if err != nil {
			return false
		}
		if st.ActiveTurn == "" && st.Phase != "busy" {
			return true
		}
		if st.ActiveTurn != "" && st.ActiveTurn != stopped {
			sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := cl.Stop(sctx, st.ActiveTurn); err != nil {
				c.logf("%s: stop turn %s: %v", name, st.ActiveTurn, err)
			}
			cancel()
			stopped = st.ActiveTurn
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(c.timing.Poll):
		}
	}
}

// watch restarts Computers that die while they should be awake.
func (c *Controller) watch(ctx context.Context) {
	for ctx.Err() == nil {
		events, err := c.drv.Watch(ctx)
		if err != nil {
			c.logf("watch: %v", err)
		} else {
			for ev := range events {
				switch ev.Type {
				case driver.EventDied:
					go c.died(ev)
				case driver.EventRemoved:
					c.removed(ev)
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// unexpected reports whether an event concerns the Computer's current
// generation while it should be awake and the Controller is not changing it.
func (c *Controller) unexpected(ev driver.Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.state.Computers[ev.Computer]
	r := c.rt[ev.Computer]
	return rec != nil && r != nil && !r.transition && !r.released && rec.DesiredPower == driver.PowerAwake && ev.Generation == rec.Generation
}

func (c *Controller) removed(ev driver.Event) {
	if c.unexpected(ev) {
		c.stopEvents(ev.Computer)
		_ = c.fail(ev.Computer, errors.New("container was removed outside a12s; run a12s up"), true)
	}
}

// died restarts an unexpectedly dead Computer with backoff: SetPower awake
// and inject again (the staged definition is unchanged). After MaxRestarts
// it stays in Error until an operator acts.
func (c *Controller) died(ev driver.Event) {
	if !c.unexpected(ev) {
		return
	}
	name := ev.Computer
	c.stopEvents(name)
	defer c.lockComputer(name)()
	if !c.unexpected(ev) {
		return
	}
	ctx := c.ctx
	if obs, ok, err := c.observeOne(ctx, name); err != nil || !ok || obs.Running || obs.Generation != ev.Generation {
		return // alive again, gone, or replaced
	}
	c.mu.Lock()
	rec := c.state.Computers[name]
	if rec.Restarts >= c.timing.MaxRestarts {
		c.mu.Unlock()
		_ = c.fail(name, fmt.Errorf("died %d times (exit %d); run a12s wake to retry", rec.Restarts+1, ev.ExitCode), true)
		return
	}
	rec.Restarts++
	n := rec.Restarts
	_ = c.saveLocked()
	c.mu.Unlock()
	c.logf("%s: died (exit %d); restart %d of %d", name, ev.ExitCode, n, c.timing.MaxRestarts)
	c.setPhase(name, api.PhaseStarting, nil)
	select {
	case <-ctx.Done():
		return
	case <-time.After(c.timing.RestartBackoff << (n - 1)):
	}
	_ = c.start(ctx, name)
}
