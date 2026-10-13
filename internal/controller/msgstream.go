package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

// The first event of a fresh Message stream subscription is a snapshot.
const (
	MsgEventSnapshot = "snapshot"
)

func terminalMsgEvent(t string) bool {
	switch t {
	case api.MsgDiscarded, api.MsgWithdrawn, api.MsgExpired, api.MsgReplaced:
		return true
	}
	return false
}

func (c *Controller) inboxNotify() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inboxBcast == nil {
		c.inboxBcast = make(chan struct{})
	}
	return c.inboxBcast
}

func (c *Controller) inboxBroadcast() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inboxBcast != nil {
		close(c.inboxBcast)
	}
	c.inboxBcast = make(chan struct{})
}

// handleMessageEvents sends a snapshot first, so a delivery or hold before the subscription
// is never missed.
func (c *Controller) handleMessageEvents(w http.ResponseWriter, r *http.Request) {
	name, id := r.PathValue("c"), r.PathValue("id")
	via := ownOnly(callerOf(r), name)
	ctx := r.Context()
	snap, err := c.inbox.Snapshot(ctx, name, id, via)
	if err != nil {
		inboxFail(w, err)
		return
	}
	cursor := snap.Cursor
	resume := false
	if v := strings.TrimSpace(r.Header.Get("Last-Event-ID")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > snap.Cursor {
			writeError(w, http.StatusBadRequest, api.ErrInvalid, "bad Last-Event-ID")
			return
		}
		cursor, resume = n, true
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	write := func(seq int64, typ string, at time.Time, data any) error {
		raw, _ := json.Marshal(data)
		ev := api.Event{V: api.EventVersion, AgentComputer: name, Seq: seq, TS: at, Type: api.EventType(typ), Data: raw}
		b, _ := json.Marshal(ev)
		if _, err := io.WriteString(w, "id: "+strconv.FormatInt(seq, 10)+"\ndata: "+string(b)+"\n\n"); err != nil {
			return err
		}
		return rc.Flush()
	}
	if !resume {
		seq := max(snap.Cursor, 1)
		if err := write(seq, MsgEventSnapshot, time.Now().UTC(), snap); err != nil {
			return
		}
		if terminalMsgEvent(snap.Message.State) {
			return
		}
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		notify := c.inboxNotify()
		evs, err := c.inbox.MessageEvents(ctx, name, id, cursor)
		if err != nil {
			return
		}
		for _, e := range evs {
			if err := write(e.ID, e.Type, e.At, e.Data); err != nil {
				return
			}
			cursor = e.ID
			if e.Message == id && terminalMsgEvent(e.Type) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-notify:
		case <-time.After(time.Second):
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
