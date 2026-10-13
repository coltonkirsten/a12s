package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

// hubRing is how many events GET /v1/events can replay to a client that reconnects.
const hubRing = 4096

// hubEvent is one event on the fleet-wide stream. Name is the agent computer it belongs to; a fleet.changed event
// has none and lists Names instead.
type hubEvent struct {
	id      int64
	name    string
	names   []string
	changed *api.FleetChangedData
	data    []byte
}

// hub fans every agent computer's events, and the Controller's own, out to GET /v1/events subscribers. Ids are
// "<boot>-<n>": a client that reconnects to another Controller run gets a gap instead of wrong events.
type hub struct {
	mu   sync.Mutex
	boot string
	next int64
	ring []hubEvent
	subs map[*hubSub]struct{}
}

type hubSub struct {
	ch   chan hubEvent
	lost atomic.Bool
}

func newHub() *hub {
	return &hub{boot: strconv.FormatInt(time.Now().UnixNano(), 36), subs: map[*hubSub]struct{}{}}
}

func (h *hub) publish(e hubEvent) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	e.id = h.next
	h.ring = append(h.ring, e)
	if len(h.ring) > hubRing {
		h.ring = slices.Delete(h.ring, 0, len(h.ring)-hubRing)
	}
	for s := range h.subs {
		select {
		case s.ch <- e:
		default:
			s.lost.Store(true)
		}
	}
}

// subscribe returns the events after lastID still in the ring, and whether some after it are gone.
func (h *hub) subscribe(lastID string) (*hubSub, []hubEvent, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &hubSub{ch: make(chan hubEvent, hubRing)}
	h.subs[s] = struct{}{}
	if lastID == "" {
		return s, nil, false
	}
	boot, n, ok := strings.Cut(lastID, "-")
	after, err := strconv.ParseInt(n, 10, 64)
	if !ok || err != nil || boot != h.boot || after > h.next {
		return s, nil, true
	}
	var replay []hubEvent
	gap := len(h.ring) > 0 && h.ring[0].id > after+1 || len(h.ring) == 0 && after < h.next
	for _, e := range h.ring {
		if e.id > after {
			replay = append(replay, e)
		}
	}
	return s, replay, gap
}

func (h *hub) unsubscribe(s *hubSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
}

func (h *hub) id(n int64) string { return h.boot + "-" + strconv.FormatInt(n, 10) }

// publishControllerEvent journals nothing: these events exist on the fleet-wide stream only.
func (c *Controller) publishControllerEvent(name string, typ api.EventType, data any, names []string, changed *api.FleetChangedData) {
	d, _ := json.Marshal(data)
	ev, _ := json.Marshal(api.Event{V: api.EventVersion, AgentComputer: name, Computer: name, TS: time.Now().UTC(), Type: typ, Data: d})
	c.hub.publish(hubEvent{name: name, names: names, changed: changed, data: ev})
}

// eventData gives a journal event its agentComputer field when an older computerd wrote it without one.
func eventData(name string, data []byte) []byte {
	if bytes.Contains(data, []byte(`"agentComputer":"`+name+`"`)) {
		return data
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return data
	}
	m["agentComputer"], _ = json.Marshal(name)
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

// handleFleetEvents is GET /v1/events: every agent computer's events, the Fleet's version changes and phase changes
// on one connection, filtered to what the caller may watch and to ?agentComputers=a,b.
func (c *Controller) handleFleetEvents(w http.ResponseWriter, r *http.Request) {
	k := callerOf(r)
	var only []string
	q := r.URL.Query().Get("agentComputers")
	if q == "" {
		q = r.URL.Query().Get("computers")
	}
	for _, n := range strings.Split(q, ",") {
		if n = strings.TrimSpace(n); n != "" {
			only = append(only, n)
		}
	}
	watches := func(name string) bool {
		return k.may(api.ActionEvents, name) && k.sees(name) && (len(only) == 0 || slices.Contains(only, name))
	}
	frame := func(e hubEvent) ([]byte, bool) {
		if e.changed == nil {
			return e.data, watches(e.name)
		}
		names := slices.DeleteFunc(slices.Clone(e.names), func(n string) bool { return !watches(n) })
		if len(names) == 0 && !(k.owner() && len(only) == 0) {
			return nil, false
		}
		if len(names) == len(e.names) {
			return e.data, true
		}
		d := *e.changed
		d.Names = names
		var ev api.Event
		_ = json.Unmarshal(e.data, &ev)
		ev.Data, _ = json.Marshal(d)
		out, _ := json.Marshal(ev)
		return out, true
	}

	sub, replay, gap := c.hub.subscribe(r.Header.Get("Last-Event-ID"))
	defer c.hub.unsubscribe(sub)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	write := func(s string) bool {
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(e hubEvent) bool {
		data, ok := frame(e)
		if !ok {
			return true
		}
		return write(fmt.Sprintf("id: %s\ndata: %s\n\n", c.hub.id(e.id), data))
	}
	if gap && !write("event: "+api.SSEGap+"\ndata: {}\n\n") {
		return
	}
	last := int64(0)
	for _, e := range replay {
		if !send(e) {
			return
		}
		last = e.id
	}
	if !write(fmt.Sprintf("event: %s\ndata: {\"id\":%q}\n\n", api.SSEHead, c.hub.id(last))) {
		return
	}
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if !write(": keepalive\n\n") {
				return
			}
		case e := <-sub.ch:
			if e.id <= last {
				continue
			}
			if sub.lost.Swap(false) && !write("event: "+api.SSEGap+"\ndata: {}\n\n") {
				return
			}
			if !send(e) {
				return
			}
		}
	}
}

// openTurn is a Turn being folded from its events.
type openTurn struct {
	t         api.Turn
	afterTool bool
	tools     map[string]int
}

// foldTurn keeps Turn history: a finished Turn is saved to fleet.db, in the shape the SDKs' turns() yields.
func (c *Controller) foldTurn(name string, ev api.Event) {
	if ev.Turn == "" {
		return
	}
	key := name + "/" + ev.Turn
	c.mu.Lock()
	if c.openTurns == nil {
		c.openTurns = map[string]*openTurn{}
	}
	o := c.openTurns[key]
	if o == nil {
		o = &openTurn{t: api.Turn{AgentComputer: name, ID: ev.Turn, Messages: []api.TurnInboxMsg{}, Tools: []api.ToolCall{}}, tools: map[string]int{}}
		c.openTurns[key] = o
	}
	var done *api.Turn
	switch ev.Type {
	case api.EventTurnStarted:
		var d api.TurnStartedData
		if json.Unmarshal(ev.Data, &d) == nil && d.Inbox != nil {
			o.t.Messages = d.Inbox.Messages
		}
		ts := ev.TS
		o.t.StartedAt = &ts
	case api.EventToolStarted:
		var d struct {
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		o.afterTool = true
		o.tools[d.ID] = len(o.t.Tools)
		o.t.Tools = append(o.t.Tools, api.ToolCall{ID: d.ID, Name: d.Name, Input: d.Input})
	case api.EventToolFinished:
		var d struct {
			ID      string          `json:"id"`
			Name    string          `json:"name"`
			Output  json.RawMessage `json:"output"`
			IsError bool            `json:"isError"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		i, ok := o.tools[d.ID]
		if !ok {
			i = len(o.t.Tools)
			o.tools[d.ID] = i
			o.t.Tools = append(o.t.Tools, api.ToolCall{ID: d.ID, Name: d.Name})
		}
		o.t.Tools[i].Output, o.t.Tools[i].IsError = d.Output, d.IsError
	case api.EventTextDelta:
		var d api.TextDeltaData
		if json.Unmarshal(ev.Data, &d) == nil && d.Text != "" {
			// Text before and after a tool call are separate messages: keep them on separate lines.
			if o.afterTool && o.t.Text != "" && !strings.HasSuffix(o.t.Text, "\n") {
				o.t.Text += "\n"
			}
			o.afterTool = false
			o.t.Text += d.Text
		}
	case api.EventTurnFinished:
		var d api.TurnFinishedData
		_ = json.Unmarshal(ev.Data, &d)
		o.t.Session, o.t.Status, o.t.Error, o.t.Usage, o.t.Tokens, o.t.FinishedAt = ev.Session, d.Status, d.Error, d.Usage, d.Tokens, ev.TS
		if len(o.t.Usage) == 0 {
			o.t.Usage = json.RawMessage("null")
		}
		t := o.t
		done = &t
		delete(c.openTurns, key)
	}
	c.mu.Unlock()
	if done != nil && c.inbox != nil {
		if err := c.inbox.SaveTurn(c.ctx, *done); err != nil {
			c.logf("%s: save turn %s: %v", name, done.ID, err)
		}
	}
}

func (c *Controller) handleTurns(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	limit := 20
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > inbox.KeepTurns {
			writeError(w, http.StatusBadRequest, api.ErrInvalid, fmt.Sprintf("limit must be 1 to %d", inbox.KeepTurns))
			return
		}
		limit = n
	}
	c.mu.Lock()
	known := c.state.Spec != nil
	if known {
		_, known = c.state.Spec.AgentComputers[name]
	}
	c.mu.Unlock()
	if !known {
		fail(w, notFound("agent computer", name))
		return
	}
	turns, err := c.inbox.Turns(r.Context(), name, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.TurnsResponse{Turns: turns})
}
