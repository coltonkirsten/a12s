package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

const (
	BatchClaimed   = "claimed"   // rows claimed; the POST may or may not have reached computerd
	BatchAccepted  = "accepted"  // computerd accepted it as Turn
	BatchDelivered = "delivered" // turn.dispatched: the harness received it
	BatchReturned  = "returned"  // settled undispatched; rows back to pending (or expired/replaced)
)

const MaxAttempts = 3

// FenceGrace is the minimum time a claimed Batch has to be accepted.
const FenceGrace = 10 * time.Second

const batchSchema = `
CREATE TABLE IF NOT EXISTS batches (
  id TEXT PRIMARY KEY,
  computer TEXT NOT NULL,
  request TEXT NOT NULL,
  claimed_at INTEGER NOT NULL,
  not_after INTEGER,
  override INTEGER NOT NULL DEFAULT 0,
  turn TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  settled_at INTEGER
);
CREATE INDEX IF NOT EXISTS batches_open ON batches (computer, state);
CREATE INDEX IF NOT EXISTS batches_turn ON batches (turn);
CREATE TABLE IF NOT EXISTS clear_ops (
  request_id TEXT PRIMARY KEY,
  computer TEXT NOT NULL,
  through_seq INTEGER NOT NULL,
  at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_journal ON messages (computer, journaled) WHERE journaled = 0;
`

type Batch struct {
	ID       string
	Computer string
	Request  api.TurnRequest
	Claimed  time.Time
	NotAfter *time.Time // the deadline fence; nil without expiring Messages
	Turn     string
	State    string
	// Override marks the one-shot session override, which Deliver consumes.
	Override bool
}

func (b *Batch) FencePassed(now time.Time) bool { return b.NotAfter != nil && now.After(*b.NotAfter) }

func scanBatch(r scanner) (*Batch, error) {
	var b Batch
	var req string
	var claimed int64
	var notAfter sql.NullInt64
	if err := r.Scan(&b.ID, &b.Computer, &req, &claimed, &notAfter, &b.Override, &b.Turn, &b.State); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(req), &b.Request); err != nil {
		return nil, fmt.Errorf("batch %s: %w", b.ID, err)
	}
	b.Claimed = fromMS(claimed)
	if notAfter.Valid {
		t := fromMS(notAfter.Int64)
		b.NotAfter = &t
	}
	return &b, nil
}

const batchCols = `id, computer, request, claimed_at, not_after, override, turn, state`

func (s *Store) OpenBatches(ctx context.Context, computer string) ([]*Batch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+batchCols+` FROM batches WHERE computer = ? AND state IN ('claimed', 'accepted') ORDER BY claimed_at, id`, computer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) BatchByTurn(ctx context.Context, turn string) (*Batch, error) {
	b, err := scanBatch(s.db.QueryRowContext(ctx, `SELECT `+batchCols+` FROM batches WHERE turn = ?`, turn))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

func (s *Store) GetBatch(ctx context.Context, id string) (*Batch, error) {
	b, err := scanBatch(s.db.QueryRowContext(ctx, `SELECT `+batchCols+` FROM batches WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

func buildRequest(computer, id, session, intro string, now time.Time, msgs []*api.Message, pendingAfter int) (api.TurnRequest, *time.Time) {
	var bms []BatchMessage
	inbox := &api.TurnInbox{Batch: id, PendingAfter: pendingAfter}
	var earliest *time.Time
	for _, m := range msgs {
		bm := BatchMessage{Frame: Frame{ID: m.ID, Sender: m.Sender, Via: m.Via, CreatedAt: m.CreatedAt, Body: *m.Body, Metadata: m.Metadata, ReplaceKey: m.ReplaceKey, Superseded: m.Superseded}, Seq: m.Seq, Bytes: m.Bytes}
		bms = append(bms, bm)
		im := api.TurnInboxMsg{ID: m.ID, Seq: m.Seq, Sender: m.Sender, Via: m.Via, CreatedAt: m.CreatedAt, Bytes: m.Bytes}
		if m.ReplaceKey != "" {
			sup := m.Superseded
			im.Key, im.Superseded = m.ReplaceKey, &sup
		}
		inbox.Messages = append(inbox.Messages, im)
		if m.ExpiresAt != nil && (earliest == nil || m.ExpiresAt.Before(*earliest)) {
			t := *m.ExpiresAt
			earliest = &t
		}
	}
	return api.TurnRequest{RequestID: id, Session: session, Prompt: RenderBatch(computer, id, intro, now, bms), Inbox: inbox}, earliest
}

// claimTx expires due Messages before the snapshot so an expired one is never
// delivered. At most one Batch per Computer is open at a time.
func (s *Store) claimTx(tx *sql.Tx, computer string, now time.Time) (*Batch, string, error) {
	if _, err := expireTx(tx, computer, now); err != nil {
		return nil, "", err
	}
	ib, err := getInbox(tx, computer)
	if err != nil {
		return nil, "", err
	}
	if ib == nil || !ib.declared {
		return nil, "not_ready", nil
	}
	if ib.held {
		return nil, "held", nil
	}
	var open int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM batches WHERE computer = ? AND state IN ('claimed', 'accepted')`, computer).Scan(&open); err != nil {
		return nil, "", err
	}
	if open > 0 {
		return nil, "busy", nil
	}
	rows, err := tx.Query(`SELECT `+messageCols+` FROM messages WHERE computer = ? AND state = 'pending' ORDER BY seq`, computer)
	if err != nil {
		return nil, "", err
	}
	var all []*api.Message
	for rows.Next() {
		m, err := scanMessage(rows, true)
		if err != nil {
			rows.Close()
			return nil, "", err
		}
		all = append(all, m)
	}
	rows.Close()
	if len(all) == 0 {
		return nil, "", nil
	}
	// A fresh Message starts a new session, so it may only lead a Batch.
	n := 1
	for n < len(all) && !all[n].Fresh {
		n++
	}
	msgs := all[:n]
	id := "b_" + NewULID(now)
	session := api.SessionCurrent
	override := ib.override == api.SessionNew
	if msgs[0].Fresh || override {
		session = api.SessionNew
	}
	var intro string
	if err := tx.QueryRow(`SELECT intro FROM inboxes WHERE computer = ?`, computer).Scan(&intro); err != nil {
		return nil, "", err
	}
	req, earliest := buildRequest(computer, id, session, intro, now, msgs, len(all)-n)
	b := &Batch{ID: id, Computer: computer, Request: req, Claimed: now, State: BatchClaimed, Override: override}
	var notAfter any
	if earliest != nil {
		fence := now.Add(FenceGrace)
		if earliest.After(fence) {
			fence = *earliest
		}
		b.NotAfter, notAfter = &fence, ms(fence)
	}
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(`INSERT INTO batches (id, computer, request, claimed_at, not_after, override, state) VALUES (?, ?, ?, ?, ?, ?, 'claimed')`,
		id, computer, string(reqJSON), ms(now), notAfter, override); err != nil {
		return nil, "", err
	}
	for _, m := range msgs {
		if _, err := tx.Exec(`UPDATE messages SET state = 'claimed', batch = ? WHERE id = ? AND state = 'pending'`, id, m.ID); err != nil {
			return nil, "", err
		}
		if err := event(tx, computer, m.ID, "claimed", now, "controller", map[string]any{"batch": id}); err != nil {
			return nil, "", err
		}
	}
	return b, "", nil
}

func (s *Store) Claim(ctx context.Context, computer string) (*Batch, error) {
	var b *Batch
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		b, _, err = s.claimTx(tx, computer, s.now())
		return err
	})
	return b, err
}

// ClaimNow runs inside the enqueue transaction so a now-or-never Message is
// either in this Batch or refused, never left queued.
func (t *Tx) ClaimNow(m api.Message) (*Batch, *Error) {
	b, reason, err := t.s.claimTx(t.tx, m.Computer, t.at)
	if err != nil {
		return nil, &Error{Status: 500, Code: api.ErrInternal, Message: err.Error()}
	}
	if b == nil {
		if reason == "" {
			reason = "not_ready"
		}
		return nil, NotIdle(m.Computer, reason)
	}
	for _, x := range b.Request.Inbox.Messages {
		if x.ID == m.ID {
			return b, nil
		}
	}
	return nil, NotIdle(m.Computer, "queue_ahead")
}

func (s *Store) Accept(ctx context.Context, batch, turn string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE batches SET turn = ?, state = 'accepted' WHERE id = ? AND state IN ('claimed', 'accepted')`, turn, batch)
	return err
}

// Deliver is called only once the harness received the Batch; settling twice
// is a no-op.
func (s *Store) Deliver(ctx context.Context, batch, turn, hold, diagnostic string) error {
	var computer string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		b, err := scanBatch(tx.QueryRow(`SELECT `+batchCols+` FROM batches WHERE id = ?`, batch))
		if err != nil {
			return err
		}
		computer = b.Computer
		if b.State == BatchDelivered || b.State == BatchReturned {
			return nil
		}
		now := s.now()
		if _, err := tx.Exec(`UPDATE batches SET state = 'delivered', turn = ?, settled_at = ? WHERE id = ?`, turn, ms(now), batch); err != nil {
			return err
		}
		ids, err := batchRows(tx, batch)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE messages SET state = 'delivered', turn = ?, ended_at = ?, end_reason = 'delivered' WHERE id = ? AND state = 'claimed'`, turn, ms(now), id); err != nil {
				return err
			}
			if err := event(tx, b.Computer, id, "delivered", now, "controller", map[string]any{"turn": turn, "batch": batch}); err != nil {
				return err
			}
		}
		if b.Override {
			if _, err := tx.Exec(`UPDATE inboxes SET session_override = '' WHERE computer = ?`, b.Computer); err != nil {
				return err
			}
		}
		if hold != "" {
			return holdTx(tx, b.Computer, hold, diagnostic, "controller", now)
		}
		return nil
	})
	if err == nil {
		s.changed(computer)
	}
	return err
}

func batchRows(tx *sql.Tx, batch string) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM messages WHERE batch = ? AND state = 'claimed' ORDER BY seq`, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func holdTx(tx *sql.Tx, computer, reason, diagnostic, by string, now time.Time) error {
	if _, err := tx.Exec(`UPDATE inboxes SET held = 1, hold_reason = ?, diagnostic = ? WHERE computer = ?`, reason, diagnostic, computer); err != nil {
		return err
	}
	return event(tx, computer, "", "held", now, by, map[string]any{"reason": reason, "diagnostic": diagnostic})
}

type ReturnOptions struct {
	// CountAttempt is false for a 409 busy or a lookup-only refusal.
	CountAttempt     bool
	Hold, Diagnostic string
	Reason           string // busy, failed, interrupted, refused
}

type ReturnResult struct {
	Pending, Expired, Replaced []string
	Held                       string
}

// Return is the one atomic return routine: rows go back to pending at their
// own seq to keep order, unless expired, cleared or replaced meanwhile.
func (s *Store) Return(ctx context.Context, batch string, o ReturnOptions) (*ReturnResult, error) {
	res := &ReturnResult{}
	var computer string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		b, err := scanBatch(tx.QueryRow(`SELECT `+batchCols+` FROM batches WHERE id = ?`, batch))
		if err != nil {
			return err
		}
		computer = b.Computer
		if b.State == BatchDelivered || b.State == BatchReturned {
			return nil
		}
		now := s.now()
		if _, err := tx.Exec(`UPDATE batches SET state = 'returned', settled_at = ? WHERE id = ?`, ms(now), batch); err != nil {
			return err
		}
		ids, err := batchRows(tx, batch)
		if err != nil {
			return err
		}
		// Expire overdue pending rows first, so a returned row is never
		// replaced by a newer one that is itself already past its deadline.
		if _, err := expireTx(tx, b.Computer, now); err != nil {
			return err
		}
		// A wake --clear-inbox accepted while this Batch was claimed still
		// covers its rows: they come back discarded, never redelivered.
		var clearThrough sql.NullInt64
		var clearID sql.NullString
		if err := tx.QueryRow(`SELECT through_seq, request_id FROM clear_ops WHERE computer = ? AND at >= ? ORDER BY through_seq DESC LIMIT 1`,
			b.Computer, ms(b.Claimed)).Scan(&clearThrough, &clearID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		undeliverable := ""
		for _, id := range ids {
			m, err := getMessage(tx, b.Computer, id, false)
			if err != nil {
				return err
			}
			attempts := m.Attempts
			if o.CountAttempt {
				attempts++
			}
			if _, err := tx.Exec(`UPDATE messages SET state = 'pending', batch = '', attempts = ? WHERE id = ?`, attempts, id); err != nil {
				return err
			}
			if err := event(tx, b.Computer, id, "returned", now, "controller", map[string]any{"batch": batch, "attempts": attempts, "reason": o.Reason}); err != nil {
				return err
			}
			switch {
			case clearThrough.Valid && m.Seq <= clearThrough.Int64:
				if err := endTx(tx, id, api.MsgDiscarded, "wake-clear", "controller", now, map[string]any{"reason": "wake-clear", "requestId": clearID.String}); err != nil {
					return err
				}
				continue
			case m.ExpiresAt != nil && !now.Before(*m.ExpiresAt):
				if err := endTx(tx, id, api.MsgExpired, "expired", "controller", now, map[string]any{"expiresAt": *m.ExpiresAt}); err != nil {
					return err
				}
				res.Expired = append(res.Expired, id)
				continue
			case m.ReplaceKey != "":
				var newer string
				var sup int64
				err := tx.QueryRow(`SELECT id, superseded FROM messages WHERE computer = ? AND via = ? AND replace_key = ? AND state = 'pending' AND id != ? ORDER BY seq DESC LIMIT 1`,
					b.Computer, m.Via, m.ReplaceKey, id).Scan(&newer, &sup)
				if err == nil {
					if err := endTx(tx, id, api.MsgReplaced, "replaced", "controller", now, map[string]any{"by": newer}); err != nil {
						return err
					}
					if _, err := tx.Exec(`UPDATE messages SET replaced_by = ? WHERE id = ?`, newer, id); err != nil {
						return err
					}
					// The survivor carries the count; its reservation was made at the widest count.
					if _, err := tx.Exec(`UPDATE messages SET superseded = ? WHERE id = ?`, sup+m.Superseded+1, newer); err != nil {
						return err
					}
					res.Replaced = append(res.Replaced, id)
					continue
				} else if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
			}
			res.Pending = append(res.Pending, id)
			if attempts >= MaxAttempts && undeliverable == "" {
				undeliverable = id
			}
		}
		switch {
		case o.Hold != "":
			res.Held = o.Hold
			return holdTx(tx, b.Computer, o.Hold, o.Diagnostic, "controller", now)
		case undeliverable != "":
			res.Held = api.HoldUndeliverable
			return holdTx(tx, b.Computer, api.HoldUndeliverable, fmt.Sprintf("message %s came back %d times without the harness receiving it", undeliverable, MaxAttempts), "controller", now)
		}
		return nil
	})
	if err == nil {
		s.changed(computer)
	}
	return res, err
}

func (s *Store) Held(ctx context.Context, computer string) (bool, string, error) {
	var held bool
	var reason string
	err := s.db.QueryRowContext(ctx, `SELECT held, hold_reason FROM inboxes WHERE computer = ?`, computer).Scan(&held, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return held, reason, err
}

func (s *Store) HasPending(ctx context.Context, computer string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE computer = ? AND state = 'pending'`, computer).Scan(&n)
	return n > 0, err
}

// MessageSnapshot is read in one transaction with the inbox_events cursor, so
// a follower resumes events from exactly this state.
type MessageSnapshot struct {
	Message    api.Message `json:"message"`
	Held       bool        `json:"held"`
	HoldReason string      `json:"holdReason,omitempty"`
	Diagnostic string      `json:"diagnostic,omitempty"`
	Cursor     int64       `json:"-"`
}

// Snapshot with via set only returns via's own Messages.
func (s *Store) Snapshot(ctx context.Context, computer, id, via string) (*MessageSnapshot, error) {
	var snap *MessageSnapshot
	err := s.tx(ctx, func(tx *sql.Tx) error {
		m, err := getMessage(tx, computer, id, false)
		if err != nil {
			return err
		}
		if via != "" && m.Via != via {
			return errf(404, api.ErrNotFound, "", "no message %s in %s's inbox", id, computer)
		}
		ib, err := getInbox(tx, computer)
		if err != nil || ib == nil {
			return errors.Join(err, ErrUnknownInbox)
		}
		snap = &MessageSnapshot{Message: *m, Held: ib.held, HoldReason: ib.reason, Diagnostic: ib.diag}
		return tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM inbox_events WHERE computer = ?`, computer).Scan(&snap.Cursor)
	})
	return snap, err
}

// MessageEvents includes the inbox's hold and resume events, since they
// affect every pending Message.
func (s *Store) MessageEvents(ctx context.Context, computer, id string, after int64) ([]InboxEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, computer, message, type, at, principal, data FROM inbox_events
		WHERE computer = ? AND id > ? AND (message = ? OR (message = '' AND type IN ('held', 'resumed'))) ORDER BY id LIMIT 1000`, computer, after, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxEvent
	for rows.Next() {
		var e InboxEvent
		var at int64
		var data string
		if err := rows.Scan(&e.ID, &e.Computer, &e.Message, &e.Type, &at, &e.Principal, &data); err != nil {
			return nil, err
		}
		e.At, e.Data = fromMS(at), json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}
