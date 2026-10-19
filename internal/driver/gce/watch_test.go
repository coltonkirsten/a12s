package gce

import (
	"context"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/driver"
)

func (h *harness) watch() <-chan driver.Event {
	h.t.Helper()
	watchEvery = 10 * time.Millisecond
	start := h.fake.Clock()
	watchStart = func() time.Time { return start }
	ctx, cancel := context.WithCancel(h.ctx)
	h.t.Cleanup(cancel)
	events, err := h.d.Watch(ctx)
	h.must(err)
	return events
}

func next(t *testing.T, events <-chan driver.Event) driver.Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return driver.Event{}
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) kill(computer string, events ...driver.Event) {
	h.guests.mu.Lock()
	v := h.guests.vms[computer]
	v.running = false
	h.guests.mu.Unlock()
	for _, ev := range events {
		v.events <- ev
	}
}

// A container that dies is reported with its generation, its exit code is recorded on the instance, and its VM is
// stopped so a dead agent computer doesn't bill; Observe then says why it stopped.
func TestDeadContainerStopsItsVM(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	events := h.watch()
	vm := InstanceName("lab", "scout")
	h.waitFor("the engine stream", func() bool { h.guests.mu.Lock(); defer h.guests.mu.Unlock(); return h.guests.opened > 0 })
	time.Sleep(50 * time.Millisecond)
	h.kill("scout", driver.Event{Type: driver.EventDied, Computer: "scout", Generation: 1, ExitCode: 1})
	if ev := next(t, events); ev.Type != driver.EventDied || ev.Computer != "scout" || ev.Generation != 1 || ev.ExitCode != 1 {
		t.Fatalf("event = %+v", ev)
	}
	h.waitFor("the VM to stop", func() bool { in, _ := h.fake.Instance(vm); return in.Status == "TERMINATED" })
	if st := h.state("scout"); st.Power != driver.PowerAsleep || st.Failure != "exited with code 1" || st.ExitCode != 1 {
		t.Fatalf("state = %+v", st)
	}
}

// An OOM kill is remembered as such, not as the exit code that follows it.
func TestOOMIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	events := h.watch()
	h.waitFor("the engine stream", func() bool { h.guests.mu.Lock(); defer h.guests.mu.Unlock(); return h.guests.opened > 0 })
	time.Sleep(50 * time.Millisecond)
	h.kill("scout",
		driver.Event{Type: driver.EventOOM, Computer: "scout", Generation: 1},
		driver.Event{Type: driver.EventDied, Computer: "scout", Generation: 1, ExitCode: 137})
	if ev := next(t, events); ev.Type != driver.EventOOM {
		t.Fatalf("event = %+v", ev)
	}
	next(t, events)
	vm := InstanceName("lab", "scout")
	h.waitFor("the VM to stop", func() bool { in, _ := h.fake.Instance(vm); return in.Status == "TERMINATED" })
	if st := h.state("scout"); !st.OOMKilled || st.Failure != "out of memory" {
		t.Fatalf("state = %+v", st)
	}
}

// GCE's own events and other principals' changes become events; the driver's own operations don't.
func TestOperationsBecomeEvents(t *testing.T) {
	h := newHarness(t)
	h.stageNew("scout")
	h.stageNew("tester")
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))
	events := h.watch()
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAsleep, 1)) // the driver's own stop
	h.must(h.d.SetPower(h.ctx, "scout", driver.PowerAwake, 1))

	h.fake.HostError(InstanceName("lab", "scout"))
	if ev := next(t, events); ev.Type != driver.EventDied || ev.Computer != "scout" || ev.Generation != 1 {
		t.Fatalf("host error event = %+v", ev)
	}
	if st := h.state("scout"); st.Power != driver.PowerAsleep || st.Failure != "host error" {
		t.Fatalf("after a host error: %+v", st)
	}
	h.fake.Act("someone@p.iam.gserviceaccount.com", "delete", InstanceName("lab", "tester"))
	if ev := next(t, events); ev.Type != driver.EventRemoved || ev.Computer != "tester" {
		t.Fatalf("delete event = %+v", ev)
	}
	select {
	case ev := <-events:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}
