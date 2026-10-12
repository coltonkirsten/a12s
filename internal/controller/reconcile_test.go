package controller

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/version"
)

var lab4 = []string{"builder", "reviewer", "scout", "tester"}

func hasCall(calls []string, prefix string) bool {
	return slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func TestUpCreatesFourAndReUpIsANoOp(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	if !slices.Equal(res.Created, lab4) || len(res.Updated)+len(res.Staged)+len(res.Removed) != 0 || res.Errors != nil {
		t.Fatalf("up = %+v", res)
	}
	for _, name := range lab4 {
		if p := h.phase(name); p != api.PhaseReady {
			t.Errorf("%s is %s", name, p)
		}
	}

	// Missing pins were resolved; the preset hash matches the directory's.
	want, _ := fleet.HashPreset("../../examples/lab/presets/scout")
	l := res.Lock
	if l.Images["slim"] != "sha256:slim1" || l.Images["browser"] != "sha256:browser1" ||
		l.Bundles["claude-code@2.1.280"] != "sha256:claude1" || l.Bundles["codex@0.154.0"] != "sha256:codex1" ||
		l.Presets["./presets/scout"] != want {
		t.Fatalf("lock = %+v (preset want %s)", l, want)
	}

	// Specs: pinned IDs run, presets and the browser seccomp profile travel.
	scout, tester := h.d.get("scout").spec, h.d.get("tester").spec
	if scout.Image != "sha256:slim1" || scout.Bundle.Image != "sha256:claude1" || len(scout.Preset) == 0 || scout.Seccomp != "" {
		t.Errorf("scout spec = %+v", scout)
	}
	if tester.Seccomp == "" || tester.CPUMillis != 2000 || tester.Memory != 4<<30 {
		t.Errorf("tester spec: seccomp %d bytes, cpu %d, mem %d", len(tester.Seccomp), tester.CPUMillis, tester.Memory)
	}

	// Credentials and TLS were injected, .generation last.
	if string(h.d.file("builder", "/run/a12s/secrets/github", "1000:1000", "0600")) != "ghp_test" {
		t.Error("builder's github credential was not injected")
	}
	for _, f := range []string{"ca.crt", "cert.pem", "key.pem"} {
		if len(h.d.file("scout", "/run/a12s/tls/"+f, "0:0", "0600")) == 0 {
			t.Errorf("scout: %s not injected", f)
		}
	}
	scoutFiles := h.d.get("scout").files
	if string(scoutFiles[secretsDir+" dir"]) != "0711" || string(scoutFiles[tlsDir+" dir"]) != "0700" {
		t.Errorf("dir modes: secrets %q, tls %q", scoutFiles[secretsDir+" dir"], scoutFiles[tlsDir+" dir"])
	}
	if g := h.d.file("scout", generationFile, "0:0", "0644"); string(g) != "1\n" {
		t.Errorf(".generation = %q", g)
	}
	calls := h.d.takeCalls()
	var scoutWrites []string
	for _, c := range calls {
		if strings.HasPrefix(c, "write scout ") {
			scoutWrites = append(scoutWrites, strings.TrimPrefix(c, "write scout "))
		}
	}
	if len(scoutWrites) != 5 || scoutWrites[4] != generationFile || scoutWrites[0] != "/run/a12s/secrets/anthropic" {
		t.Errorf("scout writes = %v", scoutWrites)
	}
	for _, d := range []string{"drive-a12s-repo", "drive-reference", "drive-ux-scratch", "home-scout"} {
		if !h.d.hasDrive(d) {
			t.Errorf("drive %s not created", d)
		}
	}
	noSecretsOnDisk(t, h.dir)

	// Re-applying the same Fleet and lock does nothing at all.
	again := h.up(labFleet(t, nil), res.Lock, false)
	if len(again.Created)+len(again.Updated)+len(again.Staged)+len(again.Removed) != 0 {
		t.Fatalf("re-up = %+v", again)
	}
	if calls := h.d.takeCalls(); len(calls) != 0 {
		t.Fatalf("re-up made calls: %v", calls)
	}
}

func fifth(f *fleet.Fleet) {
	c := f.Computers["reviewer"]
	c.Drives = nil
	f.Computers["fifth"] = c
}

func TestFifthIsAddedAloneAndRemovalKeepsDrives(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	h.d.takeCalls()

	added := h.up(labFleet(t, fifth), res.Lock, false)
	if !slices.Equal(added.Created, []string{"fifth"}) || len(added.Updated)+len(added.Staged) != 0 {
		t.Fatalf("up with a fifth = %+v", added)
	}
	for _, c := range h.d.takeCalls() {
		if !strings.Contains(c, "fifth") {
			t.Errorf("call on another computer: %s", c)
		}
	}

	removed := h.up(labFleet(t, nil), res.Lock, false)
	if !slices.Equal(removed.Removed, []string{"fifth"}) || len(removed.Created)+len(removed.Updated) != 0 {
		t.Fatalf("up without fifth = %+v", removed)
	}
	if calls := h.d.takeCalls(); !slices.Equal(calls, []string{"remove fifth"}) {
		t.Errorf("calls = %v", calls)
	}
	if !h.d.hasDrive("home-fifth") {
		t.Fatal("home-fifth was destroyed")
	}
	st := h.c.Status(context.Background())
	if len(st.OrphanedDrives) != 1 || st.OrphanedDrives[0].Name != "home-fifth" || !strings.Contains(strings.Join(st.Warnings, "\n"), "home-fifth") {
		t.Fatalf("status orphans = %+v, warnings %v", st.OrphanedDrives, st.Warnings)
	}
}

func TestBusyIsStagedThenAppliedAtIdle(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	scout := h.cds.get("scout")
	if _, err := scout.StartTurn(context.Background(), api.TurnRequest{Prompt: "work"}); err != nil {
		t.Fatal(err)
	}
	h.waitPhase("scout", api.PhaseBusy)
	h.d.takeCalls()

	bump := func(f *fleet.Fleet) {
		c := f.Computers["scout"]
		c.Harness.Model = "claude-fable-5-2"
		f.Computers["scout"] = c
	}
	staged := h.up(labFleet(t, bump), res.Lock, false)
	if !slices.Equal(staged.Staged, []string{"scout"}) || len(staged.Updated) != 0 {
		t.Fatalf("up while busy = %+v", staged)
	}
	if calls := h.d.takeCalls(); hasCall(calls, "stage scout") || hasCall(calls, "power scout") {
		t.Fatalf("busy computer was touched: %v", calls)
	}
	cs := h.c.computerStatus(context.Background(), "scout")
	if cs.Generation != 1 || cs.StagedGeneration != 2 {
		t.Fatalf("status = gen %d staged %d", cs.Generation, cs.StagedGeneration)
	}

	scout.finish(api.TurnCompleted)
	waitFor(t, func() bool { return readState(t, h.dir).Computers["scout"].Generation == 2 })
	h.waitPhase("scout", api.PhaseReady)
	calls := h.d.takeCalls()
	for _, want := range []string{"power scout asleep 1", "stage scout 2", "power scout awake 2"} {
		if !slices.Contains(calls, want) {
			t.Errorf("missing %q in %v", want, calls)
		}
	}
	if h.d.get("scout").spec.Model != "claude-fable-5-2" {
		t.Error("the new generation does not carry the change")
	}
}

func TestApplyInterrupt(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	builder := h.cds.get("builder")
	builder.StartTurn(context.Background(), api.TurnRequest{Prompt: "migrate"})
	h.waitPhase("builder", api.PhaseBusy)
	change := func(f *fleet.Fleet) {
		c := f.Computers["builder"]
		c.Egress.Allow = append(c.Egress.Allow, "proxy.golang.org")
		f.Computers["builder"] = c
	}
	if r := h.up(labFleet(t, change), res.Lock, false); !slices.Equal(r.Staged, []string{"builder"}) {
		t.Fatalf("up = %+v", r)
	}
	r, err := h.c.Apply(context.Background(), api.ApplyRequest{})
	if err != nil || !slices.Equal(r.Pending, []string{"builder"}) || len(r.Applied) != 0 {
		t.Fatalf("apply = %+v, %v", r, err)
	}
	r, err = h.c.Apply(context.Background(), api.ApplyRequest{Interrupt: true, Grace: api.Duration(time.Second)})
	if err != nil || !slices.Equal(r.Applied, []string{"builder"}) {
		t.Fatalf("apply --interrupt = %+v, %v", r, err)
	}
	if !slices.Equal(builder.stopped(), []string{"t_1"}) || h.phase("builder") != api.PhaseReady {
		t.Fatalf("stops %v, phase %s", builder.stopped(), h.phase("builder"))
	}
	if g := readState(t, h.dir).Computers["builder"].Generation; g != 2 {
		t.Fatalf("generation = %d", g)
	}
}

func TestSleepAndWake(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	scout := h.cds.get("scout")
	scout.StartTurn(context.Background(), api.TurnRequest{Prompt: "remember a word"})
	h.waitPhase("scout", api.PhaseBusy)
	h.d.takeCalls()

	if err := h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAsleep}); err != nil {
		t.Fatal(err)
	}
	if h.phase("scout") != api.PhaseAsleep || h.d.get("scout").running || !slices.Equal(scout.stopped(), []string{"t_1"}) {
		t.Fatalf("after sleep: %s running=%v stops=%v", h.phase("scout"), h.d.get("scout").running, scout.stopped())
	}
	if calls := h.d.takeCalls(); !slices.Equal(calls, []string{"power scout asleep 1"}) {
		t.Fatalf("sleep calls = %v", calls)
	}
	if _, err := h.c.awake("scout"); err == nil {
		t.Fatal("asleep computer accepts turns")
	}
	if st := readState(t, h.dir).Computers["scout"]; st.DesiredPower != driver.PowerAsleep || st.Session != "s_scout" {
		t.Fatalf("state = %+v", st)
	}

	if err := h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAwake, Credentials: labCreds}); err != nil {
		t.Fatal(err)
	}
	if h.phase("scout") != api.PhaseReady {
		t.Fatalf("after wake: %s", h.phase("scout"))
	}
	calls := h.d.takeCalls()
	if calls[0] != "power scout awake 1" || calls[len(calls)-1] != "write scout "+generationFile {
		t.Fatalf("wake calls = %v", calls)
	}
	if g := h.d.file("scout", generationFile, "0:0", "0644"); string(g) != "2\n" {
		t.Fatalf(".generation after wake = %q", g)
	}
}

func TestControllerRestartForgetsCredentials(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	if err := h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAsleep}); err != nil {
		t.Fatal(err)
	}

	// A new Controller on the same state volume attaches to what runs.
	h2 := h.restart()
	h2.waitPhase("builder", api.PhaseReady)
	h2.waitPhase("scout", api.PhaseAsleep)
	st := h2.c.Status(context.Background())
	if st.CredentialsLoaded || !strings.Contains(strings.Join(st.Warnings, "\n"), "credentials not loaded") {
		t.Fatalf("status = %+v", st)
	}
	err := h2.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAwake})
	var he *httpError
	if !errors.As(err, &he) || he.status != http.StatusBadRequest || !strings.Contains(err.Error(), "credentials missing") {
		t.Fatalf("wake without credentials = %v", err)
	}
	if cs := h2.c.computerStatus(context.Background(), "scout"); cs.Phase != api.PhaseError || !strings.Contains(cs.Error.Reason, "run a12s up") {
		t.Fatalf("scout = %+v", cs)
	}
	if err := h2.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAwake, Credentials: labCreds}); err != nil {
		t.Fatal(err)
	}
	h2.waitPhase("scout", api.PhaseReady)
}

func TestDiedIsRestartedThenErrors(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	for i := 1; i <= 3; i++ {
		h.d.takeCalls()
		h.d.kill("reviewer")
		waitFor(t, func() bool { return h.d.get("reviewer").running && h.phase("reviewer") == api.PhaseReady })
		if calls := h.d.takeCalls(); !slices.Contains(calls, "power reviewer awake 1") || !slices.Contains(calls, "write reviewer "+generationFile) {
			t.Fatalf("restart %d calls = %v", i, calls)
		}
	}
	h.d.kill("reviewer")
	h.waitPhase("reviewer", api.PhaseError)
	cs := h.c.computerStatus(context.Background(), "reviewer")
	if cs.Error == nil || !cs.Error.Retryable || h.d.get("reviewer").running {
		t.Fatalf("after 4 deaths: %+v", cs)
	}
	// Sleeping it is not a death.
	h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAsleep})
	time.Sleep(20 * time.Millisecond)
	if h.phase("scout") != api.PhaseAsleep {
		t.Fatalf("scout = %s", h.phase("scout"))
	}
}

func TestUpAsleepAppliesToNewComputersOnly(t *testing.T) {
	h := newHarness(t)
	res := h.up(labFleet(t, nil), nil, false)
	h.d.takeCalls()
	h.up(labFleet(t, fifth), res.Lock, true)
	h.waitPhase("fifth", api.PhaseAsleep)
	calls := h.d.takeCalls()
	if !slices.Contains(calls, "stage fifth 1") || hasCall(calls, "power fifth") {
		t.Fatalf("calls = %v", calls)
	}
	if h.phase("scout") != api.PhaseReady {
		t.Fatalf("scout = %s", h.phase("scout"))
	}
}

func TestAdmission(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	check := func(what string, req api.PutFleetRequest, wantMsg string) {
		t.Helper()
		_, err := h.c.PutFleet(ctx, req)
		var he *httpError
		if !errors.As(err, &he) || he.status != http.StatusBadRequest || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("%s: err = %v, want 400 containing %q", what, err, wantMsg)
		}
	}
	creds := api.Credentials{"anthropic": []byte("k"), "openai": []byte("k")}
	check("missing credential", api.PutFleetRequest{Fleet: labFleet(t, nil), Credentials: creds, Presets: labPresets(t)}, "github")
	check("missing preset", api.PutFleetRequest{Fleet: labFleet(t, nil), Credentials: labCreds}, "./presets/scout")
	lock := fleet.NewLock()
	lock.Images["slim"] = "sha256:elsewhere"
	check("pin mismatch", api.PutFleetRequest{Fleet: labFleet(t, nil), Lock: lock, Credentials: labCreds, Presets: labPresets(t)}, "sha256:elsewhere")
	other := labFleet(t, func(f *fleet.Fleet) { f.Metadata.Name = "other" })
	check("wrong fleet", api.PutFleetRequest{Fleet: other, Credentials: labCreds, Presets: labPresets(t)}, "other")
	if calls := h.d.takeCalls(); len(calls) != 0 {
		t.Fatalf("admission errors changed things: %v", calls)
	}

	// An existing pin is kept and is what runs.
	h.d.setImage("a12s/slim:dev", "sha256:slim2")
	h.d.setImage("old", "sha256:slim1")
	lock = fleet.NewLock()
	lock.Images["slim"] = "sha256:slim1"
	res := h.up(labFleet(t, nil), lock, false)
	if res.Lock.Images["slim"] != "sha256:slim1" || h.d.get("scout").spec.Image != "sha256:slim1" {
		t.Fatalf("pin changed: %v, runs %s", res.Lock.Images, h.d.get("scout").spec.Image)
	}
}

func TestIdleSleepDoesNotWaitForTheFlush(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	start := time.Now()
	if err := h.c.SetPower(context.Background(), "builder", api.PowerRequest{State: api.PowerAsleep}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d >= h.c.timing.SleepFlush {
		t.Fatalf("idle sleep took %s (flush is %s)", d, h.c.timing.SleepFlush)
	}
}

// One Computer failing readiness must not delay the others (wake --all
// sends one request per Computer, concurrently).
func TestFailingWakeDoesNotDelayOthers(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), nil, false)
	ctx := context.Background()
	for _, name := range lab4 {
		if err := h.c.SetPower(ctx, name, api.PowerRequest{State: api.PowerAsleep}); err != nil {
			t.Fatal(err)
		}
	}
	h.cds.get("tester").setBroken(true)
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- h.c.SetPower(ctx, "tester", api.PowerRequest{State: api.PowerAwake, Credentials: labCreds})
	}()
	waitFor(t, func() bool { return h.phase("tester") == api.PhaseStarting })
	for _, name := range []string{"builder", "reviewer", "scout"} {
		go h.c.SetPower(ctx, name, api.PowerRequest{State: api.PowerAwake, Credentials: labCreds})
	}
	for _, name := range []string{"builder", "reviewer", "scout"} {
		h.waitPhase(name, api.PhaseReady)
	}
	if d := time.Since(start); d >= h.c.timing.ReadyTimeout/2 {
		t.Fatalf("others were Ready only after %s", d)
	}
	if h.phase("tester") != api.PhaseStarting {
		t.Fatalf("tester = %s", h.phase("tester"))
	}
	if err := <-done; err == nil || h.phase("tester") != api.PhaseError {
		t.Fatalf("tester wake = %v, phase %s", err, h.phase("tester"))
	}
}

func TestCustomComputerImageAdmission(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.d.setImage("me/plain:1", "sha256:plain")
	h.d.setImage("me/nocomputerd:1", "sha256:nocd")
	h.d.setImage("me/agent:1", "sha256:agent")
	h.d.setImage("me/old:1", "sha256:old")
	h.d.setImage("me/rel:1", "sha256:rel")
	h.d.mu.Lock()
	h.d.computerImages = map[string]driver.ComputerImage{
		"sha256:plain": {Computerd: true},
		"sha256:nocd":  {Labels: map[string]string{driver.LabelComputerdVersion: "v0.1.0"}},
		"sha256:old":   {Labels: map[string]string{driver.LabelComputerdVersion: "v0.1.0"}, Computerd: true},
		"sha256:rel":   {Labels: map[string]string{driver.LabelComputerdVersion: "v0.2.3"}, Computerd: true},
	}
	h.d.mu.Unlock()
	withImage := func(ref string) *fleet.Fleet {
		return labFleet(t, func(f *fleet.Fleet) {
			c := f.Computers["scout"]
			c.Image = ref
			f.Computers["scout"] = c
		})
	}
	for ref, want := range map[string]string{
		"me/plain:1":       "has no ai.a12s.computerd.version label",
		"me/nocomputerd:1": "has no computerd at /opt/a12s/computerd/dist/main.js",
	} {
		_, err := h.c.PutFleet(ctx, api.PutFleetRequest{Fleet: withImage(ref), Credentials: labCreds, Presets: labPresets(t)})
		if err == nil || !strings.Contains(err.Error(), "computers.scout.image: "+ref+" "+want) || !strings.Contains(err.Error(), "fix: ") {
			t.Errorf("%s: err = %v", ref, err)
		}
	}
	res := h.up(withImage("me/agent:1"), nil, false)
	if res.Lock.Images["me/agent:1"] != "sha256:agent" {
		t.Fatalf("custom image not pinned: %v", res.Lock.Images)
	}
	if res = h.up(withImage("me/rel:1"), nil, false); !slices.ContainsFunc(res.Warnings, func(w string) bool {
		return strings.Contains(w, "computers.scout.image: me/rel:1: computerd is from a12s v0.2.3") &&
			strings.Contains(w, "dev build")
	}) {
		t.Fatalf("a dev Controller admits a release image with a warning; warnings = %q", res.Warnings)
	}

	defer func(v string) { version.Version = v }(version.Version)
	version.Version = "v0.2.0"
	h.d.setImage(version.Image(fleet.TierSlim), "sha256:slim020")
	h.d.setImage(version.Image(fleet.TierBrowser), "sha256:browser020")
	_, err := h.c.PutFleet(ctx, api.PutFleetRequest{Fleet: withImage("me/old:1"), Credentials: labCreds, Presets: labPresets(t)})
	if err == nil || !strings.Contains(err.Error(), "computers.scout.image: me/old:1: its computerd is from a12s v0.1.0, which this Controller (v0.2.0) cannot drive  fix: rebuild it FROM") {
		t.Fatalf("an older-minor 0.x image is refused: err = %v", err)
	}
}
