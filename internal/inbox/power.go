package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const powerSchema = `
CREATE TABLE IF NOT EXISTS power (
  computer TEXT PRIMARY KEY,
  slept_by TEXT NOT NULL DEFAULT '',
  slept_at INTEGER,
  paused TEXT NOT NULL DEFAULT '',
  suppressed TEXT NOT NULL DEFAULT '',
  policy TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS wakes (
  computer TEXT NOT NULL,
  at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS wakes_at ON wakes (computer, at);
`

// PowerRow is a Computer's power-policy state in fleet.db (§4.14).
type PowerRow struct {
	SleptBy    string
	SleptAt    *time.Time
	Paused     string // "" or wake_failed
	Suppressed map[string]bool
}

func ensurePower(tx *sql.Tx, computer string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO power (computer) VALUES (?)`, computer)
	return err
}

// Power returns a Computer's power state.
func (s *Store) Power(ctx context.Context, computer string) (PowerRow, error) {
	var r PowerRow
	var at sql.NullInt64
	var sup string
	err := s.db.QueryRowContext(ctx, `SELECT slept_by, slept_at, paused, suppressed FROM power WHERE computer = ?`, computer).Scan(&r.SleptBy, &at, &r.Paused, &sup)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if at.Valid {
		t := fromMS(at.Int64)
		r.SleptAt = &t
	}
	r.Suppressed = map[string]bool{}
	for _, x := range strings.Split(sup, ",") {
		if x != "" {
			r.Suppressed[x] = true
		}
	}
	return r, err
}

// SetSlept records how a Computer went to sleep ("" on a manual wake, which
// also clears a paused auto-wake) and clears the once-per-sleep suppression
// marks.
func (s *Store) SetSlept(ctx context.Context, computer, by string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ensurePower(tx, computer); err != nil {
			return err
		}
		var at any
		if by != "" {
			at = ms(s.now())
		}
		q := `UPDATE power SET slept_by = ?, slept_at = ?, suppressed = '' WHERE computer = ?`
		if by == "" {
			q = `UPDATE power SET slept_by = ?, slept_at = ?, suppressed = '', paused = '' WHERE computer = ?`
		}
		_, err := tx.Exec(q, by, at, computer)
		return err
	})
}

// SetPaused pauses or (with "") resumes automatic wakes for a Computer.
func (s *Store) SetPaused(ctx context.Context, computer, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ensurePower(tx, computer); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE power SET paused = ? WHERE computer = ?`, reason, computer)
		return err
	})
}

// ClearPaused resumes automatic wakes everywhere (a12s up).
func (s *Store) ClearPaused(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE power SET paused = ''`)
	return err
}

// PowerEvent appends a power-policy event to inbox_events.
func (s *Store) PowerEvent(ctx context.Context, computer, typ string, data any) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return event(tx, computer, "", typ, s.now(), "controller", data) })
}

// Suppress records wake.suppressed once per sleep and reason; it reports
// whether this call recorded it.
func (s *Store) Suppress(ctx context.Context, computer, reason string, data map[string]any) (bool, error) {
	recorded := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := ensurePower(tx, computer); err != nil {
			return err
		}
		var sup string
		if err := tx.QueryRow(`SELECT suppressed FROM power WHERE computer = ?`, computer).Scan(&sup); err != nil {
			return err
		}
		for _, x := range strings.Split(sup, ",") {
			if x == reason {
				return nil
			}
		}
		if _, err := tx.Exec(`UPDATE power SET suppressed = ? WHERE computer = ?`, strings.Trim(sup+","+reason, ","), computer); err != nil {
			return err
		}
		recorded = true
		if data == nil {
			data = map[string]any{}
		}
		data["reason"] = reason
		return event(tx, computer, "", "wake.suppressed", s.now(), "controller", data)
	})
	return recorded, err
}

// RecordWake counts an automatic wake and journals power.auto_wake.
func (s *Store) RecordWake(ctx context.Context, computer string, data map[string]any) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		now := s.now()
		if _, err := tx.Exec(`INSERT INTO wakes (computer, at) VALUES (?, ?)`, computer, ms(now)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM wakes WHERE at < ?`, ms(now.Add(-7*24*time.Hour))); err != nil {
			return err
		}
		return event(tx, computer, "", "power.auto_wake", now, "controller", data)
	})
}

// Wakes returns the automatic wakes of a Computer at or after since, oldest first.
func (s *Store) Wakes(ctx context.Context, computer string, since time.Time) ([]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at FROM wakes WHERE computer = ? AND at >= ? ORDER BY at`, computer, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			return nil, err
		}
		out = append(out, fromMS(at))
	}
	return out, rows.Err()
}

// WakeCandidate returns the oldest pending Message whose via is in from,
// or "".
func (s *Store) WakeCandidate(ctx context.Context, computer string, from []string) (string, string, error) {
	if len(from) == 0 {
		return "", "", nil
	}
	q := `SELECT id, via FROM messages WHERE computer = ? AND state = 'pending' AND via IN (?` + strings.Repeat(",?", len(from)-1) + `) ORDER BY seq LIMIT 1`
	args := []any{computer}
	for _, f := range from {
		args = append(args, f)
	}
	var id, via string
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&id, &via)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id, via, err
}

// SyncPolicy records a Computer's power policy; a change is power_changed.
func (s *Store) SyncPolicy(ctx context.Context, computer string, policy any, by string) error {
	b, _ := json.Marshal(policy)
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ensurePower(tx, computer); err != nil {
			return err
		}
		var old string
		if err := tx.QueryRow(`SELECT policy FROM power WHERE computer = ?`, computer).Scan(&old); err != nil {
			return err
		}
		if old == string(b) {
			return nil
		}
		if _, err := tx.Exec(`UPDATE power SET policy = ? WHERE computer = ?`, string(b), computer); err != nil {
			return err
		}
		if old == "" {
			return nil
		}
		return event(tx, computer, "", "power_changed", s.now(), by, map[string]any{"from": json.RawMessage(old), "to": json.RawMessage(b)})
	})
}
