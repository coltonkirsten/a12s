package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/computerd"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// fakeDriver is an in-memory driver.Driver. Powering a Computer awake
// brings up its fake computerd; stopping or killing it takes it down.
type fakeDriver struct {
	mu        sync.Mutex
	computers map[string]*fakeContainer
	drives    map[string]bool
	images    map[string]string // ref -> ID
	bundles   map[string]driver.Bundle
	calls     []string
	events    chan driver.Event
	cds       *fakeComputerds
}

type fakeContainer struct {
	spec    driver.ComputerSpec
	running bool
	files   map[string][]byte
}

func newFakeDriver(cds *fakeComputerds) *fakeDriver {
	bundle := func(kind, version, id string, hosts ...string) driver.Bundle {
		return driver.Bundle{ImageID: id, Manifest: driver.BundleManifest{Kind: kind, Version: version, Arch: "arm64", Bin: "bin/x", SignInHosts: hosts}}
	}
	return &fakeDriver{
		computers: map[string]*fakeContainer{},
		drives:    map[string]bool{},
		images:    map[string]string{"a12s/slim:dev": "sha256:slim1", "a12s/browser:dev": "sha256:browser1"},
		bundles: map[string]driver.Bundle{
			"a12s/bundle-claude-code:2.1.280": bundle("claude-code", "2.1.280", "sha256:claude1", "platform.claude.com"),
			"a12s/bundle-codex:0.154.0":       bundle("codex", "0.154.0", "sha256:codex1", "auth.openai.com"),
		},
		events: make(chan driver.Event, 16),
		cds:    cds,
	}
}

func (d *fakeDriver) record(format string, args ...any) {
	d.calls = append(d.calls, fmt.Sprintf(format, args...))
}

// takeCalls returns and clears the mutating calls so far.
func (d *fakeDriver) takeCalls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.calls
	d.calls = nil
	return out
}

func (d *fakeDriver) Capabilities(context.Context) (driver.Capabilities, error) {
	return driver.Capabilities{SharedWriteDrives: true, EgressEnforcement: true, Architectures: []string{"arm64"}}, nil
}

func (d *fakeDriver) Observe(context.Context) (driver.Observation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	obs := driver.Observation{Computers: map[string]driver.ComputerState{}, Drives: map[string]driver.DriveState{}}
	attached := map[string][]string{}
	for name, c := range d.computers {
		st := driver.ComputerState{Name: name, Generation: c.spec.Generation, Power: driver.PowerAsleep, Running: c.running}
		if c.running {
			st.Power = driver.PowerAwake
		}
		obs.Computers[name] = st
		attached[c.spec.HomeDrive] = append(attached[c.spec.HomeDrive], name)
		for _, m := range c.spec.Drives {
			attached[m.DriveID] = append(attached[m.DriveID], name)
		}
	}
	for id := range d.drives {
		obs.Drives[id] = driver.DriveState{ID: id, Name: "a12s-lab-" + id, AttachedTo: attached[id]}
	}
	return obs, nil
}

func (d *fakeDriver) Watch(ctx context.Context) (<-chan driver.Event, error) {
	out := make(chan driver.Event)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-d.events:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (d *fakeDriver) EnsureDrive(_ context.Context, spec driver.DriveSpec) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.drives[spec.ID] {
		return false, nil
	}
	d.drives[spec.ID] = true
	d.record("drive %s", spec.ID)
	return true, nil
}

func (d *fakeDriver) EnsureImage(_ context.Context, ref string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.images[ref]; ok {
		return id, nil
	}
	for _, id := range d.images {
		if id == ref {
			return id, nil
		}
	}
	return "", fmt.Errorf("no image %s", ref)
}

func (d *fakeDriver) EnsureBundle(_ context.Context, ref string) (driver.Bundle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.bundles[ref]; ok {
		return b, nil
	}
	for _, b := range d.bundles {
		if b.ImageID == ref {
			return b, nil
		}
	}
	return driver.Bundle{}, fmt.Errorf("no bundle %s", ref)
}

func (d *fakeDriver) StageComputer(_ context.Context, spec driver.ComputerSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.computers[spec.Name]; c != nil {
		switch {
		case c.running:
			return driver.ErrRunning
		case c.spec.Generation > spec.Generation:
			return driver.ErrStaleGeneration
		case c.spec.Generation == spec.Generation:
			return nil
		}
	}
	d.computers[spec.Name] = &fakeContainer{spec: spec, files: map[string][]byte{}}
	d.record("stage %s %d", spec.Name, spec.Generation)
	return nil
}

func (d *fakeDriver) SetPower(_ context.Context, name string, power driver.Power, gen int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.computers[name]
	switch {
	case c == nil || gen > c.spec.Generation:
		return driver.ErrNotStaged
	case gen < c.spec.Generation:
		return driver.ErrStaleGeneration
	}
	if c.running == (power == driver.PowerAwake) {
		return nil
	}
	c.running = power == driver.PowerAwake
	d.record("power %s %s %d", name, power, gen)
	if c.running {
		c.files = map[string][]byte{} // tmpfs
		d.cds.get(name).boot()
	} else {
		d.cds.get(name).halt()
	}
	return nil
}

// kill makes a running Computer die unexpectedly.
func (d *fakeDriver) kill(name string) {
	d.mu.Lock()
	c := d.computers[name]
	c.running = false
	gen := c.spec.Generation
	d.mu.Unlock()
	d.cds.get(name).halt()
	d.events <- driver.Event{Type: driver.EventDied, Computer: name, Generation: gen, ExitCode: 137}
}

func (d *fakeDriver) Connect(context.Context, string, string) (net.Conn, error) {
	return nil, fmt.Errorf("fake driver: no network")
}

func (d *fakeDriver) Exec(_ context.Context, name string, cmd []string, stdin io.Reader, asRoot bool) (int, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.computers[name]
	if c == nil || !c.running {
		return -1, "", fmt.Errorf("exec %s: not running", name)
	}
	if len(cmd) != 8 || cmd[0] != "sh" || cmd[2] != writeScript || !asRoot {
		return 1, "unexpected command", nil
	}
	data, _ := io.ReadAll(stdin)
	c.files[cmd[4]+" "+cmd[5]+" "+cmd[6]] = data
	c.files[path.Dir(cmd[4])+" dir"] = []byte(cmd[7])
	d.record("write %s %s", name, cmd[4])
	return 0, "", nil
}

func (d *fakeDriver) file(name, path, owner, mode string) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.computers[name].files[path+" "+owner+" "+mode]
}

func (d *fakeDriver) RemoveComputer(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.computers[name]; c != nil {
		if c.running {
			d.cds.get(name).halt()
		}
		delete(d.computers, name)
		d.record("remove %s", name)
	}
	return nil
}

func (d *fakeDriver) RemoveDrive(_ context.Context, id string) error {
	obs, _ := d.Observe(context.Background())
	if len(obs.Drives[id].AttachedTo) > 0 {
		return driver.ErrDriveAttached
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.drives, id)
	d.record("rm drive %s", id)
	return nil
}

// fakeComputerds holds one fake computerd per Computer name.
type fakeComputerds struct {
	mu  sync.Mutex
	all map[string]*fakeComputerd
}

func (f *fakeComputerds) get(name string) *fakeComputerd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.all == nil {
		f.all = map[string]*fakeComputerd{}
	}
	if f.all[name] == nil {
		f.all[name] = &fakeComputerd{name: name}
	}
	return f.all[name]
}

// fakeComputerd answers like computerd while its container runs.
type fakeComputerd struct {
	name string

	mu          sync.Mutex
	up          bool
	active      string
	turns       int
	seq         int64
	subs        []*fakeSub
	stops       []string
	diagnostics []string
	// keepRunningOnStop leaves the turn running when it is stopped.
	keepRunningOnStop bool
	// broken never starts listening (a computerd that fails readiness).
	broken bool
}

var errDown = fmt.Errorf("connection refused")

func (f *fakeComputerd) boot() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up, f.active = !f.broken, ""
}

func (f *fakeComputerd) setBroken(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broken = b
}

func (f *fakeComputerd) halt() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up, f.active = false, ""
	for _, sub := range f.subs {
		sub.close()
	}
	f.subs = nil
}

func (f *fakeComputerd) emitLocked(typ api.EventType, turn string, data any) {
	f.seq++
	raw, _ := json.Marshal(data)
	ev := api.Event{V: 1, Computer: f.name, Session: "s_" + f.name, Turn: turn, Seq: f.seq, TS: time.Now(), Type: typ, Data: raw}
	b, _ := json.Marshal(ev)
	msg := fmt.Sprintf("id: %d\ndata: %s\n\n", f.seq, b)
	for _, sub := range f.subs {
		select {
		case sub.ch <- msg:
		case <-sub.done:
		}
	}
}

func (f *fakeComputerd) Status(context.Context) (api.ComputerdStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return api.ComputerdStatus{}, errDown
	}
	phase := "ready"
	if f.active != "" {
		phase = "busy"
	}
	return api.ComputerdStatus{Phase: phase, Session: "s_" + f.name, ActiveTurn: f.active, Harness: api.Harness{Kind: "fake"}}, nil
}

func (f *fakeComputerd) StartTurn(_ context.Context, req api.TurnRequest) (api.TurnResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return api.TurnResponse{}, errDown
	}
	if f.active != "" {
		return api.TurnResponse{}, &computerd.Error{Status: 409, Message: "computer is busy"}
	}
	f.turns++
	f.active = fmt.Sprintf("t_%d", f.turns)
	f.emitLocked(api.EventTurnStarted, f.active, api.TurnStartedData{})
	return api.TurnResponse{TurnID: f.active, SessionID: "s_" + f.name}, nil
}

// finish ends the active turn.
func (f *fakeComputerd) finish(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active != "" {
		turn := f.active
		f.active = ""
		f.emitLocked(api.EventTurnFinished, turn, api.TurnFinishedData{Status: status})
	}
}

func (f *fakeComputerd) Stop(_ context.Context, turn string) error {
	f.mu.Lock()
	f.stops = append(f.stops, turn)
	keep := f.keepRunningOnStop
	f.mu.Unlock()
	if !keep {
		f.finish(api.TurnInterrupted)
	}
	return nil
}

func (f *fakeComputerd) Events(ctx context.Context, _, _ string) (*computerd.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return nil, errDown
	}
	pr, pw := io.Pipe()
	sub := &fakeSub{ch: make(chan string, 64), done: make(chan struct{})}
	f.subs = append(f.subs, sub)
	go func() {
		for {
			select {
			case msg := <-sub.ch:
				if _, err := pw.Write([]byte(msg)); err != nil {
					return
				}
			case <-sub.done:
				pw.Close()
				return
			case <-ctx.Done():
				pw.Close()
				return
			}
		}
	}()
	return computerd.NewStream(pr), nil
}

// fakeSub is one event stream; messages are written in order.
type fakeSub struct {
	ch   chan string
	done chan struct{}
	once sync.Once
}

func (s *fakeSub) close() { s.once.Do(func() { close(s.done) }) }

func (f *fakeComputerd) Diagnostic(_ context.Context, message string, data any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return errDown
	}
	b, _ := json.Marshal(data)
	f.diagnostics = append(f.diagnostics, message+" "+string(b))
	return nil
}

func (f *fakeComputerd) CloseIdleConnections() {}

// harness is a Controller over the fake driver.
type harness struct {
	t   *testing.T
	c   *Controller
	d   *fakeDriver
	cds *fakeComputerds
	dir string
	log *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	return startHarness(t, dir, nil)
}

func startHarness(t *testing.T, dir string, d *fakeDriver) *harness {
	t.Helper()
	cds := &fakeComputerds{}
	if d == nil {
		d = newFakeDriver(cds)
	} else {
		cds = d.cds
	}
	ca, err := LoadCA(dir, "lab")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Fleet: "lab", StateDir: dir, APIToken: "test-token-0123456789"}, d, ca)
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	c.logger = log.New(&lockedWriter{w: buf}, "", 0)
	c.timing = Timing{ReadyTimeout: 2 * time.Second, Poll: 5 * time.Millisecond, SleepFlush: 300 * time.Millisecond, RestartBackoff: time.Millisecond, MaxRestarts: 3}
	c.newClient = func(name string) computerdClient { return cds.get(name) }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Start(ctx)
	return &harness{t: t, c: c, d: d, cds: cds, dir: dir, log: buf}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// labFleet is examples/lab/fleet.yaml, optionally edited.
func labFleet(t *testing.T, edit func(f *fleet.Fleet)) *fleet.Fleet {
	t.Helper()
	f, err := fleet.Load("../../examples/lab/fleet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(f)
	}
	return f
}

func labPresets(t *testing.T) map[string][]byte {
	t.Helper()
	var buf bytes.Buffer
	if err := fleet.WritePresetTar("../../examples/lab/presets/scout", &buf); err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{"./presets/scout": buf.Bytes()}
}

var labCreds = api.Credentials{"anthropic": []byte(`{"claudeAiOauth":{}}`), "openai": []byte("sk-test"), "github": []byte("ghp_test")}

func (h *harness) up(f *fleet.Fleet, lock *fleet.Lock, asleep bool) api.PutFleetResponse {
	h.t.Helper()
	res, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: f, Lock: lock, Credentials: labCreds, Asleep: asleep, Presets: labPresets(h.t)})
	if err != nil {
		h.t.Fatalf("PutFleet: %v", err)
	}
	return res
}

func (h *harness) phase(name string) api.Phase { return h.c.phase(name) }

func (h *harness) waitPhase(name string, want api.Phase) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.phase(name) != want {
		if time.Now().After(deadline) {
			h.t.Fatalf("%s: phase %s, want %s\nlog:\n%s", name, h.phase(name), want, h.log.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func readState(t *testing.T, dir string) *State {
	t.Helper()
	st, err := loadState(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func noSecretsOnDisk(t *testing.T, dir string) {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(dir, stateFile))
	for _, v := range labCreds {
		if strings.Contains(string(data), string(v)) {
			t.Fatalf("credential material in %s", stateFile)
		}
	}
}

// get returns a copy of a container.
func (d *fakeDriver) get(name string) fakeContainer {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.computers[name]; c != nil {
		return *c
	}
	return fakeContainer{}
}

func (d *fakeDriver) hasDrive(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.drives[id]
}

func (d *fakeDriver) setImage(ref, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.images[ref] = id
}

func (f *fakeComputerd) stopped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.stops)
}
