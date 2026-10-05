package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
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
	if err := c.waitReady(ctx, name); err != nil {
		return c.fail(name, err, true)
	}
	c.subscribe(name)
	return nil
}

// waitReady polls computerd until it answers with admission open.
func (c *Controller) waitReady(ctx context.Context, name string) error {
	deadline := time.Now().Add(c.timing.ReadyTimeout)
	cl := c.client(name)
	var last error
	for {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := cl.Status(sctx)
		cancel()
		if err == nil && st.Phase != "starting" {
			c.observeStatus(name, st)
			return nil
		}
		if err == nil {
			err = errors.New("computerd is starting")
		} else if obs, ok, oerr := c.observeOne(ctx, name); oerr == nil && (!ok || !obs.Running) {
			reason := "container is not running"
			if ok && obs.Failure != "" {
				reason = "container " + obs.Failure
			}
			return errors.New(reason)
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("computerd did not become ready within %s: %v", c.timing.ReadyTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.timing.Poll):
		}
	}
}

// observeStatus takes phase, active turn, session and resumePending from
// computerd's status.
func (c *Controller) observeStatus(name string, st api.ComputerdStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	r.activeTurn, r.resumePending = st.ActiveTurn, st.ResumePending
	if rec := c.state.Computers[name]; rec != nil && st.Session != "" && rec.Session != st.Session {
		rec.Session = st.Session
		_ = c.saveLocked()
	}
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
		if err != nil {
			continue
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
	c.observeStatus(name, st)
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
	c.stopEvents(name)
	if err := c.drv.SetPower(ctx, name, driver.PowerAsleep, obs.Generation); err != nil {
		return c.fail(name, err, true)
	}
	c.setPhase(name, api.PhaseAsleep, nil)
	return nil
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
	return rec != nil && r != nil && !r.transition && rec.DesiredPower == driver.PowerAwake && ev.Generation == rec.Generation
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
