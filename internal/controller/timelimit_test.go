package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

// A task computerd stopped at its time limit reaches the agent as one inbox Message from a12s, also when the
// event is read again.
func TestTimeLimitStopTellsTheAgent(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	builder := h.cds.get("builder")
	waitFor(t, builder.subscribed)
	builder.task(true)
	seq := builder.timeLimit(7200)
	waitFor(t, func() bool { return len(builder.requestsSnapshot()) == 1 })
	req := builder.requestsSnapshot()[0]
	if req.Inbox == nil || len(req.Inbox.Messages) != 1 {
		t.Fatalf("turn request %+v", req)
	}
	m := h.message("builder", req.Inbox.Messages[0].ID)
	if m.Via != PrincipalA12s || m.Sender != "a12s" || m.Body == nil || !strings.Contains(*m.Body, `"until [ -f /tmp/never ]"`) || !strings.Contains(*m.Body, "stopped after 2h") {
		t.Fatalf("message %+v body %v", m, m.Body)
	}

	ctx := context.Background()
	evs, err := h.c.inbox.List(ctx, "builder", inbox.ListOptions{State: "all"})
	if err != nil {
		t.Fatal(err)
	}
	before := len(evs.Messages)
	data, _ := json.Marshal(api.TaskData{ID: "task", Kind: "local_bash", Description: "until [ -f /tmp/never ]", Status: api.TaskStopped, TimeLimit: 7200})
	h.c.reportTimeLimit("builder", api.Event{Seq: seq, Type: api.EventTaskFinished, Data: data})
	evs, err = h.c.inbox.List(ctx, "builder", inbox.ListOptions{State: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs.Messages) != before {
		t.Fatalf("a replayed event enqueued again: %d messages, was %d", len(evs.Messages), before)
	}

	// A task that ended any other way sends nothing.
	builder.finish(api.TurnCompleted)
	builder.task(true)
	builder.task(false)
	if evs, _ = h.c.inbox.List(ctx, "builder", inbox.ListOptions{State: "all"}); len(evs.Messages) != before {
		t.Fatalf("an ordinary task end enqueued a message")
	}
}
