// Package computerd is the Controller's client for computerd's HTTP+SSE
// contract, spoken over mTLS on a channel the
// Driver opens into the Computer.
package computerd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/sse"
)

// Dialer opens a raw connection to a Computer's computerd, for example
// driver.Connect(ctx, name, driver.ServiceComputerd).
type Dialer func(ctx context.Context) (net.Conn, error)

type Client struct {
	computer string
	tr       *http.Transport
	hc       *http.Client
}

// New returns a client for computer. tlsConfig must carry the Controller's
// client certificate and the Fleet CA as RootCAs; the server name is set to
// the Computer's name, which its certificate must carry as a DNS SAN.
func New(computer string, dial Dialer, tlsConfig *tls.Config) *Client {
	cfg := tlsConfig.Clone()
	cfg.ServerName = computer
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			raw, err := dial(ctx)
			if err != nil {
				return nil, err
			}
			conn := tls.Client(raw, cfg)
			if err := conn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, fmt.Errorf("computerd %s: tls: %w", computer, err)
			}
			return conn, nil
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Client{computer: computer, tr: tr, hc: &http.Client{Transport: tr}}
}

// CloseIdleConnections drops pooled connections, for example after the
// Computer's container was replaced.
func (c *Client) CloseIdleConnections() { c.tr.CloseIdleConnections() }

// Error is a non-2xx answer from computerd: 409 busy or conflict, 503 not
// accepting turns, 404 unknown turn.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("computerd: %d %s", e.Status, e.Message)
}

// Status is GET /v1/status.
func (c *Client) Status(ctx context.Context) (api.ComputerdStatus, error) {
	var st api.ComputerdStatus
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &st)
	return st, err
}

// StartTurn is POST /v1/turns. It returns once computerd has durably
// accepted the turn.
func (c *Client) StartTurn(ctx context.Context, req api.TurnRequest) (api.TurnResponse, error) {
	var res api.TurnResponse
	err := c.do(ctx, http.MethodPost, "/v1/turns", req, &res)
	return res, err
}

// TurnSummary is GET /v1/turns/{id}: dispatch and finish evidence for
// settling a Batch.
func (c *Client) TurnSummary(ctx context.Context, turnID string) (api.TurnSummary, error) {
	var s api.TurnSummary
	err := c.do(ctx, http.MethodGet, "/v1/turns/"+url.PathEscape(turnID), nil, &s)
	return s, err
}

// JournalInbox is POST /v1/journal/inbox: it seals inbox.discarded records
// in the journal, keyed by Message id, and returns the ids sealed.
func (c *Client) JournalInbox(ctx context.Context, records any) ([]string, error) {
	var res struct {
		Journaled []string `json:"journaled"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/journal/inbox", map[string]any{"records": records}, &res)
	return res.Journaled, err
}

// Stop is POST /v1/turns/{id}/stop; it is idempotent.
func (c *Client) Stop(ctx context.Context, turnID string) error {
	return c.do(ctx, http.MethodPost, "/v1/turns/"+url.PathEscape(turnID)+"/stop", struct{}{}, nil)
}

// CancelContinuation cancels computerd's owed restart_harness continuation
// (`inbox resume --new`); it returns the cancelled id, or "".
func (c *Client) CancelContinuation(ctx context.Context, reason string) (string, error) {
	var out struct {
		Cancelled *string `json:"cancelled"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/continuation/cancel", map[string]string{"reason": reason}, &out); err != nil {
		return "", err
	}
	if out.Cancelled == nil {
		return "", nil
	}
	return *out.Cancelled, nil
}

// Diagnostic journals a diagnostic event from the Controller (a denied
// egress request, an opened shell) under the active turn.
func (c *Client) Diagnostic(ctx context.Context, message string, data any) error {
	body := struct {
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	}{message, data}
	return c.do(ctx, http.MethodPost, "/v1/diagnostics", body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := api.MarshalWire(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://computerd"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("computerd %s: %s %s: %w", c.computer, method, path, err)
	}
	defer res.Body.Close()
	if err := checkStatus(res); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("computerd %s: %s %s: decode: %w", c.computer, method, path, err)
	}
	return nil
}

func checkStatus(res *http.Response) error {
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	return &Error{Status: res.StatusCode, Message: msg}
}

// Events opens GET /v1/events, resuming after lastEventID when it is not
// empty, and filtered to one turn when turn is not empty.
func (c *Client) Events(ctx context.Context, lastEventID, turn string) (*Stream, error) {
	u := "https://computerd/v1/events"
	if turn != "" {
		u += "?turn=" + url.QueryEscape(turn)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("computerd %s: events: %w", c.computer, err)
	}
	if err := checkStatus(res); err != nil {
		res.Body.Close()
		return nil, err
	}
	return NewStream(res.Body), nil
}

// Message is one SSE message. Journal events have an ID (the journal seq)
// and no Event name; a cursor computerd does not know gets Event api.SSEGap.
type Message struct {
	ID    string
	Event string
	Data  []byte
}

// Decode parses a journal event's data as the event envelope.
func (m Message) Decode() (api.Event, error) {
	var ev api.Event
	err := json.Unmarshal(m.Data, &ev)
	return ev, err
}

type Stream struct {
	body io.ReadCloser
	r    *sse.Reader
}

func NewStream(body io.ReadCloser) *Stream {
	return &Stream{body: body, r: sse.NewReader(body)}
}

func (s *Stream) Close() error { return s.body.Close() }

// Next returns the next complete message; comments (keepalives) are
// skipped. At the end of the stream it returns io.EOF, or
// io.ErrUnexpectedEOF when the stream ended inside a message, which is
// discarded, never returned.
func (s *Stream) Next() (Message, error) {
	f, err := s.r.Next()
	if err != nil {
		return Message{}, err
	}
	return Message{ID: f.ID, Event: f.Event, Data: f.Data}, nil
}
