package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// powerScale makes a minute 10ms and the rate window 600ms.
const powerScale = 6000

func powerFleet(t *testing.T, max int) *fleet.Fleet {
	return labFleet(t, func(f *fleet.Fleet) {
		c := f.Computers["scout"]
		idle := "1m"
		c.Power = fleet.Power{SleepAfterIdle: &idle, WakeOnMessage: &fleet.WakeOnMessage{From: []string{"computer:builder", "local"}, MaxWakesPerHour: &max}}
		f.Computers["scout"] = c
	})
}

func (h *harness) powerOf(name string) *api.PowerStatus {
	h.t.Helper()
	cs := h.c.computerStatus(context.Background(), name)
	return cs.Power
}

func (h *harness) events(name string, typ string) int {
	h.t.Helper()
	evs, err := h.c.inbox.Events(context.Background(), name, "", 0, 10000)
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// Auto-sleep after idle; a listed principal's Message wakes it and
// is delivered with everything pending; an unlisted one doesn't wake it.
func TestAutoSleepAndWakeOnMessage(t *testing.T) {
	h := startHarnessScaled(t, t.TempDir(), nil, powerScale)
	h.up(powerFleet(t, 6), false)
	h.waitPhase("scout", api.PhaseAsleep)
	if h.events("scout", "power.auto_sleep") != 1 || h.powerOf("scout").SleptBy != api.SleptAuto {
		t.Fatalf("auto sleep not recorded: %+v", h.powerOf("scout"))
	}
	other := decodeBody[api.EnqueueResponse](t, h.api("POST", "/v1/agent-computers/scout/inbox", `{"body":"from a token"}`, "Authorization", "Bearer "+h.token("ui")))
	time.Sleep(50 * time.Millisecond)
	if h.phase("scout") != api.PhaseAsleep {
		t.Fatal("an unlisted principal woke scout")
	}
	mine := h.send("scout", "from local")
	h.waitMessage("scout", mine.ID, api.MsgDelivered)
	reqs := h.cds.get("scout").requestsSnapshot()
	if len(reqs) != 1 || len(reqs[0].Inbox.Messages) != 2 || reqs[0].Inbox.Messages[0].ID != other.ID {
		t.Fatalf("wake batch %+v", reqs)
	}
	if h.events("scout", "power.auto_wake") != 1 {
		t.Fatal("auto wake not recorded")
	}
	h.cds.get("scout").finish(api.TurnCompleted)
	h.waitPhase("scout", api.PhaseAsleep) // and back to sleep after idle
}

func (h *harness) token(name string) string {
	h.t.Helper()
	res := h.api("POST", "/v1/tokens", `{"name":"`+name+`","actions":["send"],"computers":["*"]}`)
	return decodeBody[api.Token](h.t, res).Secret
}

// Things that keep it awake, and a held inbox that doesn't.
func TestIdleDefinition(t *testing.T) {
	h := startHarnessScaled(t, t.TempDir(), nil, powerScale)
	h.d.mu.Lock()
	h.d.sessions = map[string]int{"scout": 1}
	h.d.mu.Unlock()
	h.up(powerFleet(t, 6), false)
	time.Sleep(100 * time.Millisecond)
	if h.phase("scout") != api.PhaseReady {
		t.Fatal("slept with a shell attached")
	}
	h.d.mu.Lock()
	h.d.sessions = nil
	h.d.mu.Unlock()
	h.api("POST", "/v1/agent-computers/scout/inbox/hold", `{}`).Body.Close()
	h.send("scout", "held, so idle")
	h.waitPhase("scout", api.PhaseAsleep)
	time.Sleep(50 * time.Millisecond)
	if h.phase("scout") != api.PhaseAsleep {
		t.Fatal("a held inbox woke scout")
	}
	// inbox resume with a listed Message pending is an eligibility moment.
	h.api("POST", "/v1/agent-computers/scout/inbox/resume", `{}`).Body.Close()
	h.waitPhase("scout", api.PhaseBusy)
}

// The rate limit, recorded once, and the wake when the window reopens.
func TestWakeRateLimit(t *testing.T) {
	h := startHarnessScaled(t, t.TempDir(), nil, 1200) // a minute is 50ms; the window (3s) is crossed with advance
	h.up(powerFleet(t, 1), false)
	scout := h.cds.get("scout")
	scout.set(func(f *fakeComputerd) { f.autoFinish = true })
	h.waitPhase("scout", api.PhaseAsleep)
	a := h.send("scout", "first")
	h.waitMessage("scout", a.ID, api.MsgDelivered)
	h.waitPhase("scout", api.PhaseAsleep)
	b := h.send("scout", "second")
	h.send("scout", "third")
	waitFor(t, func() bool { return h.events("scout", "wake.suppressed") == 1 })
	if ps := h.powerOf("scout"); ps.AutoWake != api.AutoWakeRateLimited || ps.NextWakeAt == nil || h.message("scout", b.ID).State != api.MsgPending {
		t.Fatalf("rate limit: %+v", ps)
	}
	time.Sleep(50 * time.Millisecond) // many ticks: still recorded once
	if n := h.events("scout", "wake.suppressed"); n != 1 {
		t.Fatalf("suppressed recorded %d times", n)
	}
	h.advance(3 * time.Second)              // past the scaled hour: the window reopens by itself
	deadline := time.Now().Add(time.Second) // well inside the real 3s window
	for h.message("scout", b.ID).State != api.MsgDelivered {
		if time.Now().After(deadline) {
			t.Fatalf("not woken after the window: %+v", h.powerOf("scout"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := h.events("scout", "power.auto_wake"); n != 2 {
		t.Fatalf("auto wakes %d", n)
	}
}

// A held sleep suppresses until a manual wake; a plain sleep leaves the policy in charge.
func TestHeldSleepSuppresses(t *testing.T) {
	h := startHarnessScaled(t, t.TempDir(), nil, powerScale)
	f := powerFleet(t, 6)
	idle := "off"
	c := f.Computers["scout"]
	c.Power.SleepAfterIdle = &idle
	f.Computers["scout"] = c
	h.up(f, false)
	if res := h.api("POST", "/v1/agent-computers/scout/power", `{"state":"asleep","hold":true}`); res.StatusCode != http.StatusOK {
		t.Fatal(res.StatusCode)
	}
	m := h.send("scout", "while held asleep")
	time.Sleep(50 * time.Millisecond)
	if ps := h.powerOf("scout"); h.phase("scout") != api.PhaseAsleep || ps.AutoWake != api.AutoWakeSuppressed {
		t.Fatalf("held sleep: %s %+v", h.phase("scout"), ps)
	}
	// Survives a Controller restart.
	h2 := startHarnessScaled(t, h.dir, h.d, powerScale)
	h.stop()
	_ = h2
	h2.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f), Credentials: labCreds}, writeOpts{})
	time.Sleep(50 * time.Millisecond)
	if h2.phase("scout") != api.PhaseAsleep || h2.message("scout", m.ID).State != api.MsgPending {
		t.Fatal("a restart lost the held mark")
	}
	if err := h2.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAwake, Credentials: labCreds}); err != nil {
		t.Fatal(err)
	}
	h2.waitMessage("scout", m.ID, api.MsgDelivered)
	h2.cds.get("scout").finish(api.TurnCompleted)
	h2.waitPhase("scout", api.PhaseReady)
	h2.api("POST", "/v1/agent-computers/scout/power", `{"state":"asleep"}`).Body.Close()
	if ps := h2.powerOf("scout"); ps.SleptBy != api.SleptManual {
		t.Fatalf("plain sleep: %+v", ps)
	}
	n := h2.send("scout", "plain sleep")
	h2.waitMessage("scout", n.ID, api.MsgDelivered)
}

// After a Controller restart auto-wake pauses until a12s up, and
// expires 0 to a sleeping Computer bounces without a wake.
func TestCredentialsPaused(t *testing.T) {
	h := startHarnessScaled(t, t.TempDir(), nil, powerScale)
	f := powerFleet(t, 6)
	h.up(f, false)
	h.waitPhase("scout", api.PhaseAsleep)
	h.stop()
	h2 := startHarnessScaled(t, h.dir, h.d, powerScale)
	m := h2.send("scout", "waits")
	waitFor(t, func() bool {
		ps := h2.powerOf("scout")
		return ps != nil && ps.AutoWake == api.AutoWakePaused && ps.Reason == "credentials_not_loaded" && ps.Fix == "a12s creds set <name>"
	})
	if e := apiError(t, h2.api("POST", "/v1/agent-computers/scout/inbox", `{"body":"now","expires":0}`)); e.Details["reason"] != "asleep" {
		t.Fatalf("now-or-never asleep: %+v", e)
	}
	if h2.phase("scout") != api.PhaseAsleep {
		t.Fatal("woke without credentials")
	}
	h2.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f), Credentials: labCreds}, writeOpts{})
	h2.waitMessage("scout", m.ID, api.MsgDelivered)
}

// A wake that fails pauses auto-wake (no loop).
func TestFailedWakePauses(t *testing.T) {
	h := startHarnessScaled(t, t.TempDir(), nil, powerScale)
	h.up(powerFleet(t, 6), false)
	h.waitPhase("scout", api.PhaseAsleep)
	h.cds.get("scout").setBroken(true)
	h.send("scout", "x")
	waitFor(t, func() bool {
		ps := h.powerOf("scout")
		return ps.AutoWake == api.AutoWakePaused && ps.Reason == "wake_failed"
	})
	if h.events("scout", "power.auto_wake") != 1 {
		t.Fatalf("wakes %d", h.events("scout", "power.auto_wake"))
	}
}
