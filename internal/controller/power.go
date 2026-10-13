package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// A wake is decided on the Computer's own power policy; a sender gains no power authority.

func (c *Controller) powerSettings(name string) (fleet.PowerSettings, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.state.Fleet
	if f == nil {
		return fleet.PowerSettings{}, false
	}
	if _, ok := f.Computers[name]; !ok {
		return fleet.PowerSettings{}, false
	}
	return f.ResolvePower(name), true
}

func (c *Controller) scaled(d time.Duration) time.Duration {
	if c.cfg.TimeScale > 1 {
		return time.Duration(float64(d) / c.cfg.TimeScale)
	}
	return d
}

// timeScaleNote says when A12S_TEST_TIME_SCALE is in force: a stray value in
// the operator's shell is baked into the persistent Controller by a12s up and
// silently shortens auto-sleep, so it is logged at start and shown in status.
func (c *Controller) timeScaleNote() string {
	if c.cfg.TimeScale <= 1 {
		return ""
	}
	return fmt.Sprintf("A12S_TEST_TIME_SCALE=%g: power-policy durations run %gx fast (a test setting; unset it and run a12s down, then a12s up)", c.cfg.TimeScale, c.cfg.TimeScale)
}

func (c *Controller) credsLoaded(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	comp := c.state.Fleet.Computers[name]
	return len(missingCredentials(comp.Credentials, c.creds)) == 0
}

func (c *Controller) syncPower(ctx context.Context, f *fleet.Fleet, by string) {
	if c.inbox == nil {
		return
	}
	for _, name := range f.ComputerNames() {
		p := f.ResolvePower(name)
		if err := c.inbox.SyncPolicy(ctx, name, p, by); err != nil {
			c.logf("%s: power policy: %v", name, err)
		}
	}
}

func (c *Controller) powerLoop(ctx context.Context) {
	t := time.NewTicker(c.timing.PowerTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		var names []string
		if c.state.Fleet != nil {
			names = c.state.Fleet.ComputerNames()
		}
		c.mu.Unlock()
		for _, name := range names {
			p, ok := c.powerSettings(name)
			if !ok || !p.On() {
				c.setIdle(name, false)
				continue
			}
			switch c.phase(name) {
			case api.PhaseReady:
				if p.SleepAfterIdle == 0 {
					continue
				}
				idle := c.cheapIdle(ctx, name)
				since := c.setIdle(name, idle)
				if idle && time.Since(since) >= c.scaled(p.SleepAfterIdle) {
					c.spawnPower(name, func() { c.autoSleep(name, p) })
				}
			case api.PhaseAsleep:
				c.setIdle(name, false)
				if len(p.WakeFrom) > 0 {
					c.maybeAutoWake(ctx, name)
				}
			default:
				c.setIdle(name, false)
			}
		}
	}
}

// spawnPower runs one power action per Computer at a time.
func (c *Controller) spawnPower(name string, fn func()) {
	c.mu.Lock()
	r := c.rtLocked(name)
	if r.powerBusy {
		c.mu.Unlock()
		return
	}
	r.powerBusy = true
	c.mu.Unlock()
	if !c.spawn(func() {
		defer func() {
			c.mu.Lock()
			c.rtLocked(name).powerBusy = false
			c.mu.Unlock()
		}()
		fn()
	}) {
		c.mu.Lock()
		r.powerBusy = false
		c.mu.Unlock()
	}
}

func (c *Controller) setIdle(name string, idle bool) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.rtLocked(name)
	switch {
	case !idle:
		r.idleSince = time.Time{}
	case r.idleSince.IsZero():
		r.idleSince = time.Now()
	}
	return r.idleSince
}

// cheapIdle uses in-memory and fleet.db state only; autoSleep re-checks the rest under the lock.
func (c *Controller) cheapIdle(ctx context.Context, name string) bool {
	c.mu.Lock()
	r := c.rtLocked(name)
	busy := r.activeTurn != "" || r.background > 0 || r.closed || r.transition
	if rec := c.state.Computers[name]; rec != nil && rec.pending() {
		busy = true
	}
	c.mu.Unlock()
	if busy {
		return false
	}
	if open, err := c.inbox.OpenBatches(ctx, name); err != nil || len(open) > 0 {
		return false
	}
	held, _, _ := c.inbox.Held(ctx, name)
	if !held {
		if pending, err := c.inbox.HasPending(ctx, name); err != nil || pending {
			return false
		}
	}
	return true
}

// autoSleep sleeps a Computer whose idle clock ran out, after re-checking
// idleness under its lock (an enqueue that committed first wins).
func (c *Controller) autoSleep(name string, p fleet.PowerSettings) {
	ctx := c.ctx
	defer c.lockComputer(name)()
	if c.phase(name) != api.PhaseReady || !c.cheapIdle(ctx, name) {
		c.setIdle(name, false)
		return
	}
	// While auto-wake is paused for missing credentials, a Computer that
	// declares wakeOnMessage stays up: sleeping it would strand Messages
	// its policy promises to wake for.
	if len(p.WakeFrom) > 0 && !c.credsLoaded(name) {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	st, err := c.client(name).Status(sctx)
	cancel()
	if err != nil || st.ActiveTurn != "" || st.Phase != "ready" || st.Continuation != "" || st.Background != nil && st.Background.Tasks > 0 {
		c.setIdle(name, false)
		return
	}
	if n, err := c.drv.Sessions(ctx, name); err != nil || n > 0 {
		c.setIdle(name, false)
		return
	}
	// Close admission, then look at the inbox one last time: a Message that committed during the
	// checks above wins over the sleep; one that commits after finds the Computer asleep.
	c.closeAdmission(name)
	if held, _, _ := c.inbox.Held(ctx, name); !held {
		if pending, err := c.inbox.HasPending(ctx, name); err != nil || pending {
			c.openAdmission(name)
			c.setIdle(name, false)
			c.kick(name)
			return
		}
	}
	c.mu.Lock()
	since := c.rtLocked(name).idleSince
	c.mu.Unlock()
	data := map[string]any{"idleSince": since.UTC(), "after": p.SleepAfterIdle.String(), "cause": "auto"}
	_ = c.inbox.PowerEvent(ctx, name, "power.auto_sleep", data)
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = c.client(name).Diagnostic(dctx, "power.auto_sleep", data)
	cancel()
	if err := c.inbox.SetSlept(ctx, name, api.SleptAuto); err != nil {
		c.logf("%s: auto-sleep: %v", name, err)
		c.openAdmission(name)
		return
	}
	c.logf("%s: auto-sleep after %s idle", name, p.SleepAfterIdle)
	if err := c.sleep(ctx, name); err != nil {
		c.logf("%s: auto-sleep: %v", name, err)
	}
	c.setIdle(name, false)
}

type wakeVerdict struct {
	wake      bool
	state     string // api.AutoWake*
	reason    string
	fix       string
	candidate string
	via       string
	wakes     []time.Time
	nextAt    *time.Time
}

func (c *Controller) judgeWake(ctx context.Context, name string, p fleet.PowerSettings) wakeVerdict {
	v := wakeVerdict{state: api.AutoWakeOn}
	if len(p.WakeFrom) == 0 {
		v.state = api.AutoWakeOff
		return v
	}
	row, err := c.inbox.Power(ctx, name)
	if err != nil {
		v.state, v.reason = api.AutoWakePaused, err.Error()
		return v
	}
	v.wakes, _ = c.inbox.Wakes(ctx, name, c.now().Add(-c.scaled(time.Hour)))
	switch {
	case row.SleptBy == api.SleptHeld:
		v.state, v.reason, v.fix = api.AutoWakeSuppressed, "held", "a12s wake "+name
	case !c.credsLoaded(name):
		v.state, v.reason, v.fix = api.AutoWakePaused, "credentials_not_loaded", "a12s creds set <name>"
	case row.Paused != "":
		v.state, v.reason, v.fix = api.AutoWakePaused, row.Paused, "a12s wake "+name
	case len(v.wakes) >= p.MaxWakesPerHour:
		next := v.wakes[len(v.wakes)-p.MaxWakesPerHour].Add(c.scaled(time.Hour))
		v.state, v.reason, v.nextAt = api.AutoWakeRateLimited, "rate_limit", &next
	}
	if held, _, _ := c.inbox.Held(ctx, name); held {
		return v // a held inbox never triggers a wake
	}
	v.candidate, v.via, _ = c.inbox.WakeCandidate(ctx, name, p.WakeFrom)
	v.wake = v.candidate != "" && v.state == api.AutoWakeOn
	return v
}

// maybeAutoWake starts an automatic wake when the policy says so, and
// records a suppression once per sleep and reason when it doesn't.
func (c *Controller) maybeAutoWake(ctx context.Context, name string) {
	p, ok := c.powerSettings(name)
	if !ok || len(p.WakeFrom) == 0 || c.inbox == nil {
		return
	}
	c.mu.Lock()
	inFlight := c.rtLocked(name).powerBusy
	c.mu.Unlock()
	if inFlight || c.phase(name) != api.PhaseAsleep {
		return // a wake (or sleep) is already under way
	}
	v := c.judgeWake(ctx, name, p)
	if v.candidate == "" {
		return
	}
	if !v.wake {
		data := map[string]any{"message": v.candidate, "via": v.via}
		if v.nextAt != nil {
			data["until"] = v.nextAt.UTC()
		}
		if rec, _ := c.inbox.Suppress(ctx, name, v.reason, data); rec {
			c.logf("%s: auto-wake suppressed (%s) for %s", name, v.reason, v.candidate)
		}
		return
	}
	c.spawnPower(name, func() { c.autoWake(name, p) })
}

// autoWake wakes a Computer on its policy through the same path as a
// manual wake, with the credentials the Controller holds. A wake that fails
// pauses auto-wake until a manual wake or a12s up.
func (c *Controller) autoWake(name string, p fleet.PowerSettings) {
	ctx := c.ctx
	defer c.lockComputer(name)()
	if c.phase(name) != api.PhaseAsleep {
		return
	}
	v := c.judgeWake(ctx, name, p)
	if !v.wake {
		return
	}
	data := map[string]any{"message": v.candidate, "via": v.via, "wakesLastHour": len(v.wakes) + 1}
	if err := c.inbox.RecordWake(ctx, name, data); err != nil {
		c.logf("%s: auto-wake: %v", name, err)
		return
	}
	c.logf("%s: auto-wake for %s (via %s)", name, v.candidate, v.via)
	c.mu.Lock()
	rec := c.state.Computers[name]
	rec.DesiredPower = driver.PowerAwake
	rec.Restarts = 0
	_ = c.saveLocked()
	c.mu.Unlock()
	if err := c.inbox.SetSlept(ctx, name, ""); err != nil {
		c.logf("%s: auto-wake: %v", name, err)
	}
	obs, ok, err := c.observeOne(ctx, name)
	if err == nil {
		_, err = c.converge(ctx, name, obs, ok)
	}
	if err != nil {
		c.logf("%s: auto-wake failed: %v; auto-wake paused until a12s wake %s or a12s up", name, err, name)
		_ = c.inbox.SetPaused(ctx, name, "wake_failed")
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = c.client(name).Diagnostic(dctx, "power.auto_wake", data)
	cancel()
}

func (c *Controller) powerStatus(ctx context.Context, name string, phase api.Phase) *api.PowerStatus {
	p, ok := c.powerSettings(name)
	if !ok || !p.On() || c.inbox == nil {
		return nil
	}
	ps := &api.PowerStatus{SleepAfterIdle: "off", WakeOnMessage: p.WakeFrom, MaxWakesPerHour: p.MaxWakesPerHour, AutoWake: api.AutoWakeOff}
	if p.SleepAfterIdle > 0 {
		ps.SleepAfterIdle = p.SleepAfterIdle.String()
	}
	if row, err := c.inbox.Power(ctx, name); err == nil && phase == api.PhaseAsleep {
		ps.SleptBy, ps.SleptAt = row.SleptBy, row.SleptAt
	}
	v := c.judgeWake(ctx, name, p)
	ps.AutoWake, ps.Reason, ps.Fix, ps.WakesLastHour, ps.NextWakeAt = v.state, v.reason, v.fix, len(v.wakes), v.nextAt
	if ps.AutoWake == api.AutoWakeOn && len(p.WakeFrom) == 0 {
		ps.AutoWake = api.AutoWakeOff
	}
	c.mu.Lock()
	if s := c.rtLocked(name).idleSince; !s.IsZero() {
		t := s.UTC()
		ps.IdleSince = &t
	}
	c.mu.Unlock()
	return ps
}
