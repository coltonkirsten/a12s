package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

// Home drive use crossing 90% reaches the agent as one inbox Message from a12s, also when the event is read again;
// falling back under 80% sends nothing.
func TestDiskHighTellsTheAgentOnce(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	builder := h.cds.get("builder")
	waitFor(t, builder.subscribed)
	seq := builder.diskUsage(api.DiskHigh, 9<<30+1<<29, 10<<30)
	waitFor(t, func() bool { return len(builder.requestsSnapshot()) == 1 })
	req := builder.requestsSnapshot()[0]
	if req.Inbox == nil || len(req.Inbox.Messages) != 1 {
		t.Fatalf("turn request %+v", req)
	}
	m := h.message("builder", req.Inbox.Messages[0].ID)
	if m.Via != PrincipalA12s || m.Sender != "a12s" || m.Body == nil || !strings.Contains(*m.Body, "95% (9.5 GiB of 10.0 GiB)") || !strings.Contains(*m.Body, "Clean up now") {
		t.Fatalf("message %+v body %v", m, m.Body)
	}
	builder.finish(api.TurnCompleted)

	ctx := context.Background()
	count := func() int {
		evs, err := h.c.inbox.List(ctx, "builder", inbox.ListOptions{State: "all"})
		if err != nil {
			t.Fatal(err)
		}
		return len(evs.Messages)
	}
	before := count()
	data, _ := json.Marshal(api.DiskUsageData{Level: api.DiskHigh, DiskUsage: api.DiskUsage{UsedBytes: 9<<30 + 1<<29, TotalBytes: 10 << 30}})
	h.c.reportDiskHigh("builder", api.Event{Seq: seq, Type: api.EventDiskUsage, Data: data})
	if n := count(); n != before {
		t.Fatalf("a replayed event enqueued again: %d messages, was %d", n, before)
	}
	builder.diskUsage(api.DiskNormal, 7<<30, 10<<30)
	waitFor(t, func() bool { return h.c.computerStatus(ctx, "builder").Phase == api.PhaseReady })
	if n := count(); n != before {
		t.Fatal("falling back under 80% enqueued a message")
	}
}

// A computerd whose journal can't be written shows as Error "home disk full", not as a crash, and comes back by itself.
func TestDiskFullIsAStatusNotACrash(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	builder := h.cds.get("builder")
	waitFor(t, builder.subscribed)
	ctx := context.Background()

	builder.set(func(f *fakeComputerd) { f.disk = &api.DiskUsage{UsedBytes: 92 << 28, TotalBytes: 100 << 28} })
	cs := h.c.computerStatus(ctx, "builder")
	if !slices.Contains(cs.Warnings, "home disk 92% (23.0 GiB of 25.0 GiB) full; raise resources.disk or free space") {
		t.Fatalf("warnings at 92%% = %q", cs.Warnings)
	}

	builder.fillDisk(true)
	waitFor(t, func() bool { return h.c.phase("builder") == api.PhaseError })
	cs = h.c.computerStatus(ctx, "builder")
	if cs.Error == nil || !strings.HasPrefix(cs.Error.Reason, "home disk full") || !cs.Error.Retryable {
		t.Fatalf("status of a full disk = %+v", cs.Error)
	}
	builder.fillDisk(false)
	waitFor(t, func() bool { return h.c.phase("builder") == api.PhaseReady })
	if cs := h.c.computerStatus(ctx, "builder"); cs.Error != nil {
		t.Fatalf("error after recovery: %+v", cs.Error)
	}
}

// resources.disk sizes the home drive: a new size reaches the Driver without restaging the agent computer.
func TestResourcesDiskSizesTheHomeDrive(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, func(f *fleet.Fleet) {
		c := f.Computers["builder"]
		c.Resources.Disk = 30 << 30
		f.Computers["builder"] = c
	}), false)
	home := driver.HomeDriveID("builder")
	size := func() int64 {
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		return h.d.sizes[home]
	}
	if size() != 30<<30 {
		t.Fatalf("home drive asked for %d bytes, want 30 GiB", size())
	}
	h.d.takeCalls()
	h.up(labFleet(t, func(f *fleet.Fleet) {
		c := f.Computers["builder"]
		c.Resources.Disk = 40 << 30
		f.Computers["builder"] = c
	}), false)
	if size() != 40<<30 {
		t.Fatalf("home drive asked for %d bytes after the change, want 40 GiB", size())
	}
	if calls := h.d.takeCalls(); hasCall(calls, "stage builder") || hasCall(calls, "power builder") {
		t.Fatalf("a new disk size restaged the agent computer: %v", calls)
	}
}
