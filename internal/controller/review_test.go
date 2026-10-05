package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// #24: a prompt being admitted when sleep starts is seen (and stopped) by
// sleep; a prompt after sleep has closed admission is refused.
func TestPromptAdmissionIsSerializedWithSleep(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	scout := h.cds.get("scout")
	gate := make(chan struct{})
	scout.setGate(gate)

	turn := make(chan int, 1)
	go func() {
		res := h.api("POST", "/v1/computers/scout/turns", `{"requestId":"r1","session":"current","prompt":"work"}`)
		res.Body.Close()
		turn <- res.StatusCode
	}()
	waitFor(t, func() bool { // the prompt holds admission inside StartTurn
		h.c.mu.Lock()
		defer h.c.mu.Unlock()
		r := h.c.rtLocked("scout")
		if !r.admit.TryLock() {
			return true
		}
		r.admit.Unlock()
		return false
	})
	slept := make(chan error, 1)
	go func() { slept <- h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAsleep}) }()
	time.Sleep(20 * time.Millisecond)
	if h.phase("scout") == api.PhaseStopping {
		t.Fatal("sleep did not wait for the prompt being admitted")
	}
	scout.setGate(nil)
	close(gate)
	if code := <-turn; code != 200 {
		t.Fatalf("admitted prompt = %d", code)
	}
	if err := <-slept; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(scout.stopped(), []string{"t_1"}) {
		t.Fatalf("sleep did not interrupt the admitted turn: stops %v", scout.stopped())
	}
	res := h.api("POST", "/v1/computers/scout/turns", `{"requestId":"r2","session":"current","prompt":"x"}`)
	if e := apiError(t, res); res.StatusCode != http.StatusServiceUnavailable || e.Code != api.ErrAsleep {
		t.Fatalf("after sleep = %d %+v", res.StatusCode, e)
	}
}

// #24: restaging an idle Computer closes admission before its idle check.
func TestRestageClosesAdmission(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	gate := make(chan struct{})
	h.cds.get("reviewer").setGate(gate)
	go h.api("POST", "/v1/computers/reviewer/turns", `{"requestId":"r1","session":"current","prompt":"x"}`)
	time.Sleep(20 * time.Millisecond)
	done := make(chan api.PutFleetResponse, 1)
	go func() {
		done <- h.up(labFleet(t, func(f *fleet.Fleet) {
			c := f.Computers["reviewer"]
			c.Harness.Model = "gpt-7"
			f.Computers["reviewer"] = c
		}), res.Lock, false)
	}()
	select {
	case r := <-done:
		t.Fatalf("restage did not wait for the prompt being admitted: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	h.cds.get("reviewer").setGate(nil)
	close(gate)
	if r := <-done; !slices.Equal(r.Staged, []string{"reviewer"}) {
		t.Fatalf("up = %+v (the admitted turn makes reviewer busy)", r)
	}
}

// #25: an exclusive drive moved from a busy Computer waits until the old
// holder's generation is gone; there are never two attachments.
func TestExclusiveDriveMoveWaitsForTheHolder(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	tester := h.cds.get("tester")
	tester.StartTurn(context.Background(), api.TurnRequest{RequestID: "r1", Prompt: "ux"})
	h.waitPhase("tester", api.PhaseBusy)

	move := func(f *fleet.Fleet) {
		tc, sc := f.Computers["tester"], f.Computers["scout"]
		sc.Drives = append(sc.Drives, tc.Drives...)
		tc.Drives = nil
		f.Computers["tester"], f.Computers["scout"] = tc, sc
	}
	staged := h.up(labFleet(t, move), res.Lock, false)
	if !slices.Equal(staged.Staged, []string{"scout", "tester"}) {
		t.Fatalf("up = %+v", staged)
	}
	if m := h.d.mounts("drive-ux-scratch"); !slices.Equal(m, []string{"tester"}) {
		t.Fatalf("ux-scratch attached to %v while tester is busy", m)
	}
	tester.finish(api.TurnCompleted)
	waitFor(t, func() bool { return slices.Equal(h.d.mounts("drive-ux-scratch"), []string{"scout"}) })
	h.waitPhase("scout", api.PhaseReady)
	h.waitPhase("tester", api.PhaseReady)
	for _, name := range []string{"scout", "tester"} {
		if readState(t, h.dir).Computers[name].pending() {
			t.Errorf("%s still pending", name)
		}
	}
}

// #26: a stage the Driver reports but Observe does not show is not
// recorded as done.
func TestStageIsReadBack(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	h.d.setLostStages(true)
	out := h.up(labFleet(t, func(f *fleet.Fleet) {
		c := f.Computers["builder"]
		c.Harness.Model = "gpt-7"
		f.Computers["builder"] = c
	}), res.Lock, false)
	if !strings.Contains(out.Errors["builder"], "not observed") {
		t.Fatalf("errors = %v", out.Errors)
	}
	if g := readState(t, h.dir).Computers["builder"].Generation; g != 1 {
		t.Fatalf("generation %d recorded without being observed", g)
	}
	if h.phase("builder") != api.PhaseError {
		t.Fatalf("builder = %s", h.phase("builder"))
	}
}

// #28: a restart finishes removing Computers the Fleet dropped.
func TestRestartFinishesRemovals(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, fifth), nil, false)
	st := readState(t, h.dir) // as if the Controller died right after saving the smaller Fleet
	delete(st.Fleet.Computers, "fifth")
	if err := st.save(filepath.Join(h.dir, stateFile)); err != nil {
		t.Fatal(err)
	}
	startHarness(t, h.dir, h.d)
	waitFor(t, func() bool { return h.d.get("fifth").spec.Name == "" })
	if !h.d.hasDrive("home-fifth") {
		t.Fatal("home-fifth was destroyed")
	}
}

// #29: presets survive a restart; without the bytes, staging is refused.
func TestPresetsSurviveRestart(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	h.d.RemoveComputer(context.Background(), "scout") // lost: must be staged again
	h2 := startHarness(t, h.dir, h.d)
	h2.waitPhase("scout", api.PhaseError) // no credentials after the restart, but staged
	if len(h.d.get("scout").spec.Preset) == 0 {
		t.Fatal("scout was staged without its preset")
	}

	h.d.RemoveComputer(context.Background(), "scout")
	if err := os.RemoveAll(filepath.Join(h.dir, presetDir)); err != nil {
		t.Fatal(err)
	}
	h3 := startHarness(t, h.dir, h.d)
	h3.waitPhase("scout", api.PhaseError)
	if h.d.get("scout").spec.Name != "" {
		t.Fatal("scout was staged without preset bytes")
	}
	if cs := h3.c.computerStatus(context.Background(), "scout"); !strings.Contains(cs.Error.Reason, "not stored") {
		t.Fatalf("scout error = %+v", cs.Error)
	}
}

// #30: a staged egress change waits with the rest of the generation.
func TestEgressFollowsTheActiveGeneration(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	scout := h.cds.get("scout")
	scout.StartTurn(context.Background(), api.TurnRequest{RequestID: "r1", Prompt: "x"})
	h.waitPhase("scout", api.PhaseBusy)
	h.up(labFleet(t, func(f *fleet.Fleet) {
		c := f.Computers["scout"]
		c.Egress.Allow = []string{"new.example"}
		f.Computers["scout"] = c
	}), res.Lock, false)
	p, _ := h.c.policy("scout")
	if p.Check("new.example", 443) == "" || p.Check("api.anthropic.com", 443) != "" || p.Check("platform.claude.com", 443) != "" {
		t.Fatalf("policy changed before the staged generation applied: %+v", p)
	}
	scout.finish(api.TurnCompleted)
	waitFor(t, func() bool { return readState(t, h.dir).Computers["scout"].Generation == 2 })
	p, _ = h.c.policy("scout")
	if p.Check("new.example", 443) != "" || p.Check("api.anthropic.com", 443) == "" || p.Check("platform.claude.com", 443) != "" {
		t.Fatalf("policy after apply: %+v", p)
	}
}

// #32: retrying a finished request does not make the Computer Busy.
func TestIdempotentRetryIsNotBusy(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	body := `{"requestId":"r1","session":"current","prompt":"hi"}`
	h.api("POST", "/v1/computers/scout/turns", body).Body.Close()
	h.waitPhase("scout", api.PhaseBusy)
	h.cds.get("scout").finish(api.TurnCompleted)
	h.waitPhase("scout", api.PhaseReady)
	res := h.api("POST", "/v1/computers/scout/turns", body)
	var tr api.TurnResponse
	json.NewDecoder(res.Body).Decode(&tr)
	res.Body.Close()
	if tr.TurnID != "t_1" {
		t.Fatalf("retry = %+v", tr)
	}
	time.Sleep(20 * time.Millisecond)
	if h.phase("scout") != api.PhaseReady {
		t.Fatalf("scout = %s after an idempotent retry", h.phase("scout"))
	}
}

// #37: after a Controller restart, credentials sent with up are injected
// into running Computers.
func TestCredentialsAreInjectedAfterRestart(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	h2 := startHarness(t, h.dir, h.d)
	h2.waitPhase("builder", api.PhaseReady)
	h.d.takeCalls()
	fresh := api.Credentials{"anthropic": []byte(`{"new":1}`), "openai": []byte("sk-new"), "github": []byte("ghp_new")}
	if _, err := h2.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: labFleet(t, nil), Lock: res.Lock, Credentials: fresh, Presets: labPresets(t)}); err != nil {
		t.Fatal(err)
	}
	if got := h.d.file("builder", "/run/a12s/secrets/openai", "1000:1000", "0600"); string(got) != "sk-new" {
		t.Fatalf("builder openai = %q", got)
	}
	// Known material is not injected again.
	h.d.takeCalls()
	h2.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: labFleet(t, nil), Lock: res.Lock, Credentials: fresh, Presets: labPresets(t)})
	if calls := h.d.takeCalls(); hasCall(calls, "write") {
		t.Fatalf("re-injected unchanged material: %v", calls)
	}
}

// #43: the session a turn switched to is kept while asleep.
func TestAsleepShowsTheLatestSession(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	h.api("POST", "/v1/computers/scout/turns", `{"requestId":"r1","session":"new","prompt":"hi"}`).Body.Close()
	waitFor(t, func() bool { return readState(t, h.dir).Computers["scout"].Session == "s_scout_1" })
	h.cds.get("scout").finish(api.TurnCompleted)
	h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAsleep})
	if cs := h.c.computerStatus(context.Background(), "scout"); cs.Phase != api.PhaseAsleep || cs.Session != "s_scout_1" {
		t.Fatalf("asleep status = %+v", cs)
	}
}

// #46: status carries each Computer's active egress mode.
func TestStatusEgressMode(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, func(f *fleet.Fleet) {
		c := f.Computers["tester"]
		c.Egress.Allow = []string{"*"}
		f.Computers["tester"] = c
	}), nil, false)
	st := h.c.Status(context.Background())
	got := map[string]string{}
	for _, cs := range st.Computers {
		got[cs.Name] = cs.EgressMode
	}
	if got["tester"] != api.EgressAllowAll || got["scout"] != api.EgressEnforced {
		t.Fatalf("egress = %v", got)
	}

	u := newHarness(t)
	u.up(labFleet(t, func(f *fleet.Fleet) { f.Egress.Enforcement = fleet.EnforcementUnenforced }), nil, false)
	if cs := u.c.computerStatus(context.Background(), "scout"); cs.EgressMode != api.EgressUnenforced {
		t.Fatalf("unenforced = %q", cs.EgressMode)
	}
}
