package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/sse"
)

// GapError is returned when the server answers a cursor with an SSE `gap`
// event: the events after After are no longer (or not yet) in the journal.
type GapError struct {
	After int64
	Data  string
}

func (e *GapError) Error() string {
	msg := fmt.Sprintf("journal gap after seq %d: events were lost", e.After)
	if e.Data != "" {
		msg += " (" + e.Data + ")"
	}
	return msg
}

// Stream reads a Controller event stream (GET /v1/computers/{c}/events) and
// reconnects with Last-Event-ID whenever the connection drops, so each event
// is delivered exactly once, in seq order.
type Stream struct {
	Client *Client
	// Path is the request path including any query, e.g.
	// /v1/computers/scout/events?turn=t_1.
	Path string
	// After is the cursor: the last seq already seen. On the first connect
	// it is sent as Last-Event-ID only when SendCursor is set (a ?turn=
	// stream without a cursor replays that turn from its start); on every
	// reconnect the last delivered seq is sent.
	After      int64
	SendCursor bool
	// UntilHead ends the stream at the replay boundary, computerd's `head`
	// event (`a12s logs` without --follow). A connection that ends before
	// it is ErrReplayIncomplete, not a reconnect. Live streams ignore head.
	UntilHead bool
	// GiveUp bounds how long reconnecting may keep failing. Zero means 2m.
	GiveUp time.Duration
	// OnReconnect, if set, is told about each dropped connection.
	OnReconnect func(after int64, err error)
}

// MaxMalformed is how many times in a row a stream may deliver a malformed
// event after the same cursor before Run gives up.
const MaxMalformed = 3

// malformedError is an event that is not a valid journal event (bad JSON,
// no seq or no type). The stream reconnects from the last good cursor.
type malformedError struct {
	after   int64
	id      string
	payload string
}

func (e *malformedError) Error() string {
	return fmt.Sprintf("malformed event after seq %d (id %q): %s", e.after, e.id, Truncate(e.payload, 120))
}

// ErrReplayIncomplete is returned by an UntilHead stream that ended before
// the replay boundary.
var ErrReplayIncomplete = errors.New("replay incomplete")

// Run delivers events to fn until fn returns stop or an error, the context
// ends, or the server reports a gap.
func (s *Stream) Run(ctx context.Context, fn func(api.Event) (stop bool, err error)) error {
	giveUp := s.GiveUp
	if giveUp == 0 {
		giveUp = 2 * time.Minute
	}
	last := s.After
	sendCursor := s.SendCursor
	backoff := 100 * time.Millisecond
	var failingSince time.Time
	malformed := 0 // consecutive malformed events after the same cursor
	for {
		delivered, done, err := s.connect(ctx, last, sendCursor, fn)
		if delivered > last {
			last = delivered
			failingSince = time.Time{}
			backoff = 100 * time.Millisecond
			malformed = 0
		}
		if done {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var bad *malformedError
		if errors.As(err, &bad) {
			if malformed++; malformed >= MaxMalformed {
				return fmt.Errorf("event stream: giving up after %d attempts: %w", malformed, err)
			}
		} else if s.UntilHead {
			return fmt.Errorf("%w: %v", ErrReplayIncomplete, err)
		}
		if failingSince.IsZero() {
			failingSince = time.Now()
		} else if time.Since(failingSince) > giveUp {
			return fmt.Errorf("event stream: giving up after %s: %w", giveUp, err)
		}
		if s.OnReconnect != nil {
			s.OnReconnect(last, err)
		}
		sendCursor = sendCursor || last > 0
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

// connect runs one connection. It returns the last seq it delivered, whether
// the stream is finished (fn stopped, head reached, gap or fn error), and the error.
func (s *Stream) connect(ctx context.Context, after int64, sendCursor bool, fn func(api.Event) (bool, error)) (int64, bool, error) {
	req, err := s.Client.NewRequest(ctx, http.MethodGet, s.Path, nil)
	if err != nil {
		return after, true, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if sendCursor {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	resp, err := s.Client.HTTP.Do(req)
	if err != nil {
		return after, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := ResponseError(resp)
		if resp.StatusCode >= 500 && resp.StatusCode != http.StatusServiceUnavailable {
			return after, false, err // the Controller may be restarting its upstream
		}
		return after, true, err
	}

	last := after
	r := sse.NewReader(resp.Body)
	for {
		f, err := r.Next()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF // a live stream does not end by itself
			}
			return last, false, fmt.Errorf("event stream dropped after seq %d: %w", last, err)
		}
		ev, payload, evID := f.Event, string(f.Data), f.ID
		switch ev {
		case "":
		case api.SSEGap:
			return last, true, &GapError{After: last, Data: payload}
		case api.SSEHead:
			if s.UntilHead {
				return last, true, nil
			}
			continue
		default:
			continue // a named event this client does not know
		}
		if payload == "" && evID == "" {
			continue // keep-alive
		}
		var e api.Event
		if err := json.Unmarshal([]byte(payload), &e); err != nil || e.Seq == 0 || e.Type == "" {
			// Never skip it: reconnect from the last good cursor.
			return last, false, &malformedError{after: last, id: evID, payload: payload}
		}
		if e.Seq <= last {
			continue // replayed after a reconnect: already delivered
		}
		last = e.Seq
		stop, err := fn(e)
		if stop || err != nil {
			return last, true, err
		}
	}
}
