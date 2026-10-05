package controller

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/computerd"
)

const goodEvent1 = "id: 1\ndata: {\"v\":1,\"seq\":1,\"type\":\"text.delta\",\"data\":{\"text\":\"x\"}}\n\n"

// The reconnect cursor moves only past events that decoded: a damaged event,
// or an incomplete one cut off by EOF, is fetched again next time.
func TestReadEventsCursorAdvancesOnlyOnDecodedEvents(t *testing.T) {
	for name, tail := range map[string]string{
		"damaged":    "id: 2\ndata: {not json\n\n",
		"incomplete": "id: 2\ndata: {\"v\":1,\"seq\":2,\"type\":\"turn.fin",
	} {
		c := &Controller{rt: map[string]*runtime{}}
		c.readEvents("scout", computerd.NewStream(io.NopCloser(strings.NewReader(goodEvent1+tail))))
		if got := c.rt["scout"].lastEventID; got != "1" {
			t.Errorf("%s: cursor = %q, want 1", name, got)
		}
	}
}

// eventsClient serves canned streams and records the cursor of each open.
type eventsClient struct {
	computerdClient // only Events and Status are used
	mu              sync.Mutex
	bodies          []string
	cursors         []string
	cancel          context.CancelFunc
}

func (e *eventsClient) Status(context.Context) (api.ComputerdStatus, error) {
	return api.ComputerdStatus{}, io.EOF // skip the status observation
}

func (e *eventsClient) Events(_ context.Context, last, _ string) (*computerd.Stream, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cursors = append(e.cursors, last)
	if len(e.cursors) == len(e.bodies) {
		e.cancel()
	}
	body := e.bodies[len(e.cursors)-1]
	return computerd.NewStream(io.NopCloser(strings.NewReader(body))), nil
}

// A malformed event followed by a valid one: the stream stops at the bad
// event, the cursor stays on the last good one, and the stream is re-opened
// from there instead of continuing past it.
func TestFollowEventsReopensAtMalformedEvent(t *testing.T) {
	for name, bad := range map[string]string{
		"bad json":     "id: 2\ndata: {not json\n\n",
		"missing seq":  "id: 2\ndata: {\"v\":1,\"type\":\"text.delta\"}\n\n",
		"missing type": "id: 2\ndata: {\"v\":1,\"seq\":2}\n\n",
	} {
		ctx, cancel := context.WithCancel(context.Background())
		valid3 := "id: 3\ndata: {\"v\":1,\"seq\":3,\"type\":\"text.delta\",\"data\":{\"text\":\"y\"}}\n\n"
		cl := &eventsClient{bodies: []string{goodEvent1 + bad + valid3, ""}, cancel: cancel}
		c := &Controller{
			rt:        map[string]*runtime{},
			clients:   map[string]computerdClient{},
			newClient: func(string) computerdClient { return cl },
			timing:    Timing{Poll: time.Millisecond},
		}
		c.followEvents(ctx, "scout")
		if got := c.rt["scout"].lastEventID; got != "1" {
			t.Errorf("%s: cursor = %q, want 1", name, got)
		}
		if strings.Join(cl.cursors, ",") != ",1" {
			t.Errorf("%s: opens with cursors %q, want [\"\" \"1\"]", name, cl.cursors)
		}
	}
}
