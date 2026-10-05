package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
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
	// Idle, when positive, ends the stream (without error) once no event
	// has arrived for that long: `a12s logs` without --follow.
	Idle time.Duration
	// GiveUp bounds how long reconnecting may keep failing. Zero means 2m.
	GiveUp time.Duration
	// OnReconnect, if set, is told about each dropped connection.
	OnReconnect func(after int64, err error)
}

// errIdle cancels a request when the idle timer fires.
var errIdle = errors.New("idle")

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
	for {
		delivered, done, err := s.connect(ctx, last, sendCursor, fn)
		if delivered > last {
			last = delivered
			failingSince = time.Time{}
			backoff = 100 * time.Millisecond
		}
		if done {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
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
// the stream is finished (fn stopped, idle, gap or fn error), and the error.
func (s *Stream) connect(ctx context.Context, after int64, sendCursor bool, fn func(api.Event) (bool, error)) (int64, bool, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := s.Client.NewRequest(ctx, http.MethodGet, s.Path, nil)
	if err != nil {
		return after, true, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if sendCursor {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	var idle *time.Timer
	if s.Idle > 0 {
		idle = time.AfterFunc(s.Idle, func() { cancel(errIdle) })
		defer idle.Stop()
	}
	resp, err := s.Client.HTTP.Do(req)
	if err != nil {
		if errors.Is(context.Cause(ctx), errIdle) {
			return after, true, nil
		}
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
	r := bufio.NewReader(resp.Body)
	var id, event string
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(context.Cause(ctx), errIdle) {
				return last, true, nil
			}
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return last, false, fmt.Errorf("event stream dropped after seq %d: %w", last, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "id":
				id = value
			case "event":
				event = value
			case "data":
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(value)
			}
			continue
		}
		// A blank line dispatches the event.
		ev, payload := event, data.String()
		evID := id
		id, event = "", ""
		data.Reset()
		if ev == api.SSEGap {
			return last, true, &GapError{After: last, Data: payload}
		}
		if payload == "" {
			continue // keep-alive or comment
		}
		var e api.Event
		if err := json.Unmarshal([]byte(payload), &e); err != nil {
			return last, true, fmt.Errorf("event stream: bad event %q: %w", payload, err)
		}
		if e.Seq == 0 {
			e.Seq, _ = strconv.ParseInt(evID, 10, 64)
		}
		if e.Seq <= last {
			continue // replayed after a reconnect: already delivered
		}
		last = e.Seq
		if idle != nil {
			idle.Reset(s.Idle)
		}
		stop, err := fn(e)
		if stop || err != nil {
			return last, true, err
		}
	}
}
