package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

// Discarded is the sealed-journal record of a Message that ended without
// delivery (§4.10): inbox.discarded with its body.
type Discarded struct {
	ID         string            `json:"id"`
	Seq        int64             `json:"seq"`
	Sender     string            `json:"sender"`
	Via        string            `json:"via"`
	CreatedAt  time.Time         `json:"createdAt"`
	Body       string            `json:"body"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Reason     string            `json:"reason"` // wake-clear | inbox-clear | withdrawn | expired | replaced
	By         string            `json:"by"`
	ExpiresAt  *time.Time        `json:"expiresAt,omitempty"`
	ReplacedBy string            `json:"replacedBy,omitempty"`
}

// Unjournaled returns a Computer's ended Messages not yet sealed in its
// journal, oldest first, up to about maxBytes of bodies.
func (s *Store) Unjournaled(ctx context.Context, computer string, maxBytes int) ([]Discarded, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, seq, sender, via, created_at, body, metadata, end_reason, ended_by, expires_at, replaced_by
		FROM messages WHERE computer = ? AND journaled = 0 ORDER BY seq LIMIT 500`, computer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Discarded
	size := 0
	for rows.Next() {
		var d Discarded
		var created int64
		var meta string
		var exp sql.NullInt64
		if err := rows.Scan(&d.ID, &d.Seq, &d.Sender, &d.Via, &created, &d.Body, &meta, &d.Reason, &d.By, &exp, &d.ReplacedBy); err != nil {
			return nil, err
		}
		d.CreatedAt = fromMS(created)
		if meta != "" {
			_ = json.Unmarshal([]byte(meta), &d.Metadata)
		}
		if exp.Valid {
			t := fromMS(exp.Int64)
			d.ExpiresAt = &t
		}
		if len(out) > 0 && size+len(d.Body) > maxBytes {
			break
		}
		size += len(d.Body)
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkJournaled records that these Messages are sealed in the journal.
func (s *Store) MarkJournaled(ctx context.Context, ids []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE messages SET journaled = 1 WHERE id = ?`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Prune deletes terminal rows older than the retention that are sealed in
// the journal, their events and settled Batches (§4.3).
func (s *Store) Prune(ctx context.Context) (int64, error) {
	cutoff := ms(s.now().Add(-Retention))
	var n int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM messages WHERE ended_at IS NOT NULL AND ended_at < ? AND journaled = 1 AND state NOT IN ('pending', 'claimed')`, cutoff)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		if _, err := tx.Exec(`DELETE FROM inbox_events WHERE at < ? AND (message = '' OR message NOT IN (SELECT id FROM messages))`, cutoff); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM batches WHERE settled_at IS NOT NULL AND settled_at < ?`, cutoff)
		return err
	})
	return n, err
}

// WakeClear is a wake's clear-inbox operation (§4.7): recorded with the
// inbox head at acceptance under the client's requestId, then applied. A
// retried request with the same id is the same operation, so it never
// clears a Message that arrived later.
func (s *Store) WakeClear(ctx context.Context, computer, requestID, by string) (int64, int, error) {
	var through int64
	n := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var c string
		err := tx.QueryRow(`SELECT computer, through_seq FROM clear_ops WHERE request_id = ?`, requestID).Scan(&c, &through)
		switch {
		case err == nil:
			if c != computer {
				return errf(409, api.ErrConflict, "use a new requestId", "requestId %s was used for %s", requestID, c)
			}
		case errors.Is(err, sql.ErrNoRows):
			ib, err := getInbox(tx, computer)
			if err != nil {
				return err
			}
			if ib == nil {
				return ErrUnknownInbox
			}
			through = ib.nextSeq - 1
			if _, err := tx.Exec(`INSERT INTO clear_ops (request_id, computer, through_seq, at) VALUES (?, ?, ?, ?)`, requestID, computer, through, ms(s.now())); err != nil {
				return err
			}
		default:
			return err
		}
		rows, err := tx.Query(`SELECT id FROM messages WHERE computer = ? AND state = 'pending' AND seq <= ? ORDER BY seq`, computer, through)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		now := s.now()
		for _, id := range ids {
			if err := endTx(tx, id, api.MsgDiscarded, "wake-clear", by, now, map[string]any{"reason": "wake-clear", "requestId": requestID}); err != nil {
				return err
			}
		}
		n = len(ids)
		return nil
	})
	if err == nil && n > 0 {
		s.changed(computer)
	}
	return through, n, err
}
