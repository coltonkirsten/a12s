package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Discarded is the sealed-journal record of a Message that ended without
// delivery.
type Discarded struct {
	ID         string            `json:"id"`
	Seq        int64             `json:"seq"`
	Sender     string            `json:"sender"`
	Via        string            `json:"via"`
	CreatedAt  time.Time         `json:"createdAt"`
	Body       string            `json:"body"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Reason     string            `json:"reason"` // inbox-clear | withdrawn | expired | replaced
	By         string            `json:"by"`
	ExpiresAt  *time.Time        `json:"expiresAt,omitempty"`
	ReplacedBy string            `json:"replacedBy,omitempty"`
}

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
		// Budget the serialized record (JSON escaping can grow a body
		// several times), so a chunk always fits computerd's request limit.
		enc, _ := json.Marshal(d)
		n := len(enc) + 64
		if len(out) > 0 && size+n > maxBytes {
			break
		}
		size += n
		out = append(out, d)
	}
	return out, rows.Err()
}

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

// Prune never deletes an unjournaled row, so a discarded body is sealed
// before it is lost.
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
