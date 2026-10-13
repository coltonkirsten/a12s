package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// computerImages overrides what InspectComputerImage reports for an image ID.
	computerImages map[string]driver.ComputerImage
	bundles        map[string]driver.Bundle
	calls          []string
	events         chan driver.Event
	cds            *fakeComputerds
	// lostStages makes StageComputer report success without effect;
	// ghostStarts makes Observe never show a container running (a start
	// the substrate reported but that did not take).
	lostStages, ghostStarts bool
	// exclusive drives (as in the lab Fleet); staging a second attachment
	// of one is recorded in violations.
	exclusive  map[string]bool
	violations []string
	// ips is the Controller's address on each Computer network (by
	// Computer), allocated when a network first appears, freed with it.
	ips     map[string]netip.Addr
	addrErr error
	// beforeRemove runs at the start of RemoveComputer (the network still
	// exists).
	beforeRemove func(name string)
	// observeGate, when set, holds the next Observe until it is closed.
	observeGate chan struct{}
	// sessions are attached shell sessions, by Computer.
	sessions map[string]int
	// services are Service containers; svcImages what each image declares;
	// svcUnhealthy fails a Service's exec health check.
	services     map[string]*fakeService
	svcImages    map[string]driver.ServiceImage
	svcUnhealthy map[string]bool
	logins       map[string]driver.LoginSpec
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
		images:    map[string]string{"a12s/slim:dev": "sha256:slim1"},
		bundles: map[string]driver.Bundle{
			"a12s/bundle-claude-code:2.1.280": bundle("claude-code", "2.1.280", "sha256:claude1", "platform.claude.com"),
			"a12s/bundle-codex:0.154.0":       bundle("codex", "0.154.0", "sha256:codex1", "auth.openai.com"),
		},
		events:       make(chan driver.Event, 16),
		cds:          cds,
		exclusive:    map[string]bool{"drive-a12s-repo": true, "drive-ux-scratch": true},
		services:     map[string]*fakeService{},
		svcImages:    map[string]driver.ServiceImage{},
		svcUnhealthy: map[string]bool{},
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
	return driver.Capabilities{SharedWriteDrives: true, EgressEnforcement: true, Architectures: []string{"arm64"}, Services: true}, nil
}

func (d *fakeDriver) Observe(context.Context) (driver.Observation, error) {
	d.mu.Lock()
	gate := d.observeGate
	d.observeGate = nil
	d.mu.Unlock()
	if gate != nil {
		<-gate
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	obs := driver.Observation{Computers: map[string]driver.ComputerState{}, Drives: map[string]driver.DriveState{}, Services: map[string]driver.ComputerState{}}
	for name, s := range d.services {
		st := driver.ComputerState{Name: name, Generation: s.spec.Generation, Power: driver.PowerAsleep, Running: s.running}
		if s.running {
			st.Power = driver.PowerAwake
			st.Address = "10.213.200." + fmt.Sprint(len(name))
		}
		obs.Services[name] = st
	}
	attached := map[string][]string{}
	for name, c := range d.computers {
		running := c.running && !d.ghostStarts
		st := driver.ComputerState{Name: name, Generation: c.spec.Generation, Power: driver.PowerAsleep, Running: running}
		if running {
			st.Power = driver.PowerAwake
		}
		obs.Computers[name] = st
		attached[c.spec.HomeDrive] = append(attached[c.spec.HomeDrive], name)
		for _, m := range c.spec.Drives {
			attached[m.DriveID] = append(attached[m.DriveID], name)
		}
	}
	for name, sv := range d.services {
		if sv.spec.StateDrive != "" {
			attached[sv.spec.StateDrive] = append(attached[sv.spec.StateDrive], name)
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

func (d *fakeDriver) InspectComputerImage(_ context.Context, id string) (driver.ComputerImage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if img, ok := d.computerImages[id]; ok {
		return img, nil
	}
	return driver.ComputerImage{Labels: map[string]string{driver.LabelComputerdVersion: "dev"}, Computerd: true}, nil
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
	if d.lostStages {
		return nil
	}
	for _, m := range spec.Drives {
		for other, c := range d.computers {
			if other != spec.Name && d.exclusive[m.DriveID] && slices.ContainsFunc(c.spec.Drives, func(o driver.Mount) bool { return o.DriveID == m.DriveID }) {
				d.violations = append(d.violations, fmt.Sprintf("%s staged with %s while %s attaches it", spec.Name, m.DriveID, other))
			}
		}
	}
	d.computers[spec.Name] = &fakeContainer{spec: spec, files: map[string][]byte{}}
	d.record("stage %s %d", spec.Name, spec.Generation)
	if d.ips == nil {
		d.ips = map[string]netip.Addr{}
	}
	if _, ok := d.ips[spec.Name]; !ok { // the lowest free /29, as the Docker driver allocates
	next:
		for i := 0; ; i++ {
			ip := netip.AddrFrom4([4]byte{10, 213, byte(i * 8 >> 8), byte(i*8 + 2)})
			for _, used := range d.ips {
				if used == ip {
					continue next
				}
			}
			d.ips[spec.Name] = ip
			break
		}
	}
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
	if d.beforeRemove != nil {
		d.beforeRemove(name)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.computers[name]; c != nil {
		if c.running {
			d.cds.get(name).halt()
		}
		delete(d.computers, name)
		delete(d.ips, name) // RemoveComputer deletes the network too
		d.record("remove %s", name)
	}
	return nil
}

func (d *fakeDriver) Sessions(_ context.Context, name string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions[name], nil
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
	// session is the current session id ("" means "s_<name>").
	session  string
	requests map[string]string // requestId -> turn
	// gate, when set, holds StartTurn until it is closed.
	gate chan struct{}
	// reqs are the accepted requests, in order; dispatched and finished
	// record each turn's receipt and outcome.
	reqs       []api.TurnRequest
	dispatched map[string]bool
	finished   map[string]string
	// noDispatch makes turns start without receipt (the harness died first);
	// resumeFail makes them fail with resume.failed before receipt.
	noDispatch, resumeFail bool
	refused                map[string]bool
	continuation           string
	// conflictNext answers the next StartTurn 409 busy (a turn that raced
	// the drainer); autoFinish completes every turn right after it starts.
	conflictNext, autoFinish bool
	// sealed are the inbox.discarded records journaled, by Message id.
	sealed map[string]map[string]any
}

func (f *fakeComputerd) JournalInbox(_ context.Context, records any) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return nil, errDown
	}
	b, _ := json.Marshal(records)
	var recs []map[string]any
	json.Unmarshal(b, &recs)
	if f.sealed == nil {
		f.sealed = map[string]map[string]any{}
	}
	var ids []string
	for _, r := range recs {
		id, _ := r["id"].(string)
		if _, ok := f.sealed[id]; !ok {
			f.sealed[id] = r
			f.emitLocked(api.EventInboxDiscarded, "", r)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (f *fakeComputerd) sealedSnapshot() map[string]map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]any{}
	for k, v := range f.sealed {
		out[k] = v
	}
	return out
}

func (f *fakeComputerd) sessionLocked() string {
	if f.session == "" {
		return "s_" + f.name
	}
	return f.session
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
	ev := api.Event{V: 1, Computer: f.name, Session: f.sessionLocked(), Turn: turn, Seq: f.seq, TS: time.Now(), Type: typ, Data: raw}
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
	return api.ComputerdStatus{Phase: phase, Session: f.sessionLocked(), ActiveTurn: f.active, Harness: api.Harness{Kind: "fake"}, Continuation: f.continuation}, nil
}

func (f *fakeComputerd) TurnSummary(_ context.Context, turn string) (api.TurnSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return api.TurnSummary{}, errDown
	}
	for rid, t := range f.requests {
		if t == turn {
			return api.TurnSummary{TurnID: turn, RequestID: rid, Dispatched: f.dispatched[turn], Finished: f.finished[turn], ResumeFailed: f.resumeFail && !f.dispatched[turn] && f.finished[turn] != ""}, nil
		}
	}
	return api.TurnSummary{}, &computerd.Error{Status: 404, Message: "unknown turn"}
}

func (f *fakeComputerd) requestsSnapshot() []api.TurnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.TurnRequest(nil), f.reqs...)
}

// dispatch reports receipt for the active turn (with noDispatch set).
func (f *fakeComputerd) dispatch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active != "" && !f.dispatched[f.active] {
		f.dispatched[f.active] = true
		f.emitLocked(api.EventTurnDispatched, f.active, map[string]any{})
	}
}

func (f *fakeComputerd) StartTurn(_ context.Context, req api.TurnRequest) (api.TurnResponse, error) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return api.TurnResponse{}, errDown
	}
	if turn, ok := f.requests[req.RequestID]; ok && req.RequestID != "" {
		return api.TurnResponse{TurnID: turn, SessionID: f.sessionLocked()}, nil // idempotent retry
	}
	if f.refused[req.RequestID] {
		return api.TurnResponse{}, &computerd.Error{Status: 404, Message: "unknown_request"}
	}
	if req.LookupOnly {
		if f.refused == nil {
			f.refused = map[string]bool{}
		}
		f.refused[req.RequestID] = true
		return api.TurnResponse{}, &computerd.Error{Status: 404, Message: "unknown_request"}
	}
	if f.active != "" || f.conflictNext {
		f.conflictNext = false
		return api.TurnResponse{}, &computerd.Error{Status: 409, Message: "computer is busy"}
	}
	if req.Continuation && req.RequestID != f.continuation {
		return api.TurnResponse{}, &computerd.Error{Status: 409, Message: "no continuation owed"}
	}
	f.turns++
	f.active = fmt.Sprintf("t_%d", f.turns)
	if f.requests == nil {
		f.requests = map[string]string{}
	}
	f.requests[req.RequestID] = f.active
	f.reqs = append(f.reqs, req)
	if f.dispatched == nil {
		f.dispatched, f.finished = map[string]bool{}, map[string]string{}
	}
	f.emitLocked(api.EventTurnStarted, f.active, api.TurnStartedData{RequestID: req.RequestID, Inbox: req.Inbox})
	if req.Session == api.SessionNew {
		from := f.sessionLocked()
		f.session = fmt.Sprintf("s_%s_%d", f.name, f.turns)
		f.emitLocked(api.EventSessionChanged, f.active, map[string]string{"from": from, "to": f.session})
	}
	if f.resumeFail {
		turn := f.active
		f.active = ""
		f.finished[turn] = api.TurnFailed
		f.emitLocked(api.EventResumeFailed, turn, map[string]string{"reason": "damaged"})
		f.emitLocked(api.EventTurnFinished, turn, api.TurnFinishedData{Status: api.TurnFailed})
		return api.TurnResponse{TurnID: turn, SessionID: f.sessionLocked()}, nil
	}
	if !f.noDispatch {
		f.dispatched[f.active] = true
		if req.Continuation {
			f.continuation = ""
		}
		f.emitLocked(api.EventTurnDispatched, f.active, map[string]any{})
	}
	if f.autoFinish {
		go f.finish(api.TurnCompleted)
	}
	return api.TurnResponse{TurnID: f.active, SessionID: f.sessionLocked()}, nil
}

func (f *fakeComputerd) set(fn func(f *fakeComputerd)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeComputerd) setGate(g chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = g
}

func (f *fakeComputerd) finish(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active != "" {
		turn := f.active
		f.active = ""
		if f.finished == nil {
			f.finished = map[string]string{}
		}
		f.finished[turn] = status
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

func (f *fakeComputerd) EgressDenied(_ context.Context, d api.EgressDeniedData) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return errDown
	}
	b, _ := json.Marshal(d)
	f.diagnostics = append(f.diagnostics, "egress.denied "+string(b))
	return nil
}

func (f *fakeComputerd) CloseIdleConnections() {}

// harness is a Controller over the fake driver.
type harness struct {
	t    *testing.T
	c    *Controller
	d    *fakeDriver
	cds  *fakeComputerds
	dir  string
	log  *bytes.Buffer
	stop func()
	// skew moves the power clock and fleet.db's clock forward (advance).
	skew *atomic.Int64
}

// advance moves the Controller's power clock and fleet.db's clock forward,
// so a test need not wait out a real window.
func (h *harness) advance(d time.Duration) { h.skew.Add(int64(d)) }

// persistCreds names the state dirs whose harness keeps credentials on disk; the rest run memory-only, so their
// restarts forget credentials.
var persistCreds = map[string]bool{}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	return startHarness(t, dir, nil)
}

func startHarness(t *testing.T, dir string, d *fakeDriver) *harness {
	t.Helper()
	return startHarnessScaled(t, dir, d, 1)
}

// startHarnessScaled runs the power policy at a test speed: durations are
// divided by scale.
func startHarnessScaled(t *testing.T, dir string, d *fakeDriver, scale float64) *harness {
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
	c, err := New(Config{Fleet: "lab", StateDir: dir, APIToken: "test-token-0123456789", TimeScale: scale, MemoryOnlyCredentials: !persistCreds[dir]}, d, ca)
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	c.logger = log.New(&lockedWriter{w: buf}, "", 0)
	c.timing = Timing{ReadyTimeout: 2 * time.Second, Poll: 5 * time.Millisecond, SleepFlush: 300 * time.Millisecond, RestartBackoff: time.Millisecond, MaxRestarts: 3, ReadBack: time.Second, PowerTick: 5 * time.Millisecond, HealthPoll: 5 * time.Millisecond, HealthStartup: 300 * time.Millisecond, HealthInterval: 20 * time.Millisecond}
	c.newClient = func(name string) computerdClient { return cds.get(name) }
	skew := &atomic.Int64{}
	c.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	c.inbox.SetClock(c.now)
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, c: c, d: d, cds: cds, dir: dir, log: buf, skew: skew}
	h.stop = sync.OnceFunc(func() {
		cancel()
		c.Wait() // nothing of this Controller runs on after it stops
	})
	t.Cleanup(h.stop)
	c.Start(ctx)
	return h
}

// restart stops this Controller and starts a new one on the same state
// dir and Driver, as a Controller container restart does.
func (h *harness) restart() *harness {
	h.stop()
	return startHarness(h.t, h.dir, h.d)
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

// specFor is what a12s up sends for f: each agent computer as written is its merged form, with the lab's preset
// inlined as files.
func specFor(f *fleet.Fleet) *fleet.Spec {
	s := &fleet.Spec{APIVersion: f.APIVersion, Kind: f.Kind, Metadata: f.Metadata, Egress: f.Egress, Drives: f.Drives,
		Services: f.Services, AgentComputers: map[string]json.RawMessage{}}
	if d := f.Defaults; d.Home.Preset == "" {
		if raw := fleet.SpecOf(d); string(raw) != "{}" {
			s.Defaults = raw
		}
	}
	for name, c := range f.Computers {
		if p := c.Home.Preset; p != "" {
			files, err := fleet.InlineFiles(filepath.Join("../../examples/lab", p))
			if err != nil {
				panic(err)
			}
			c.Files, c.Home.Preset = files, ""
		}
		s.AgentComputers[name] = fleet.SpecOf(c)
	}
	return s
}

var labCreds = api.Credentials{"anthropic": []byte(`{"claudeAiOauth":{}}`), "openai": []byte("sk-test"), "github": []byte("ghp_test")}

func (h *harness) up(f *fleet.Fleet, lock *fleet.Lock, asleep bool) api.PutFleetResponse {
	h.t.Helper()
	res, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f), Lock: lock, Credentials: labCreds, Asleep: asleep, File: true}, writeOpts{})
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

func (d *fakeDriver) setLostStages(b bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lostStages = b
}

// mounts reports which containers attach a drive.
func (d *fakeDriver) mounts(driveID string) []string {
	obs, _ := d.Observe(context.Background())
	return obs.Drives[driveID].AttachedTo
}

func (d *fakeDriver) setGhostStarts(b bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ghostStarts = b
}

// mute makes computerd stop answering while its container keeps running.
func (f *fakeComputerd) mute() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up = false
}

func (d *fakeDriver) twoWriters() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.violations)
}

// controllerAddrs is the Docker driver's ControllerAddrs over the fake.
func (d *fakeDriver) controllerAddrs(context.Context) (map[string]netip.Addr, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.addrErr != nil {
		return nil, d.addrErr
	}
	out := map[string]netip.Addr{"a12s-lab": netip.MustParseAddr("172.19.0.2")}
	for name, ip := range d.ips {
		out["a12s-lab-"+name] = ip
	}
	return out, nil
}

func (d *fakeDriver) ip(name string) netip.Addr {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ips[name]
}

func (d *fakeDriver) setIP(name string, ip netip.Addr) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ips[name] = ip
}

func (d *fakeDriver) setAddrErr(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addrErr = err
}

func (f *fakeComputerd) CancelContinuation(_ context.Context, reason string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.continuation
	if id != "" && f.active == "" {
		f.continuation = ""
		f.emitLocked(api.EventContinuationAbandoned, "", map[string]any{"continuation": id, "reason": reason, "cancelled": true})
		return id, nil
	}
	return "", nil
}
