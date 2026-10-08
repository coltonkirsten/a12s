package inbox

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// serviceSchema is Service state that outlives the Controller (§4.1): each
// Service's desired power, and its audit lines (`a12s service logs`).
const serviceSchema = `
CREATE TABLE IF NOT EXISTS service_power (
  service TEXT PRIMARY KEY,
  desired TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS service_audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  service TEXT NOT NULL,
  at INTEGER NOT NULL,
  line TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS service_audit_by ON service_audit (service, id);
`

// ServiceAuditKeep is how many audit lines are kept per Service.
const ServiceAuditKeep = 2000

// ServiceDesired returns a Service's desired power ("awake" or "asleep"),
// and false if none is recorded.
func (s *Store) ServiceDesired(ctx context.Context, service string) (string, bool, error) {
	var d string
	err := s.db.QueryRowContext(ctx, `SELECT desired FROM service_power WHERE service = ?`, service).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return d, err == nil, err
}

// SetServiceDesired records a Service's desired power.
func (s *Store) SetServiceDesired(ctx context.Context, service, desired string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO service_power (service, desired) VALUES (?, ?)
ON CONFLICT (service) DO UPDATE SET desired = excluded.desired`, service, desired)
	return err
}

// ForgetService drops a removed Service's power state (its audit lines
// stay until pruned).
func (s *Store) ForgetService(ctx context.Context, service string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM service_power WHERE service = ?`, service)
	return err
}

// AuditLine is one Controller audit line about a Service.
type AuditLine struct {
	ID   int64
	At   time.Time
	Line string
}

// Audit appends an audit line for a Service, keeping the newest
// ServiceAuditKeep.
func (s *Store) Audit(ctx context.Context, service, line string) (AuditLine, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO service_audit (service, at, line) VALUES (?, ?, ?)`, service, ms(now), line)
	if err != nil {
		return AuditLine{}, err
	}
	id, _ := res.LastInsertId()
	if id%100 == 0 {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM service_audit WHERE service = ? AND id <= ?`, service, id-ServiceAuditKeep)
	}
	return AuditLine{ID: id, At: now, Line: line}, nil
}

// AuditLines returns a Service's audit lines after id, oldest first, at
// most limit (0: all kept).
func (s *Store) AuditLines(ctx context.Context, service string, after int64, limit int) ([]AuditLine, error) {
	if limit <= 0 {
		limit = ServiceAuditKeep
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, line FROM (SELECT id, at, line FROM service_audit WHERE service = ? AND id > ? ORDER BY id DESC LIMIT ?) ORDER BY id`, service, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditLine
	for rows.Next() {
		var l AuditLine
		var at int64
		if err := rows.Scan(&l.ID, &at, &l.Line); err != nil {
			return nil, err
		}
		l.At = fromMS(at)
		out = append(out, l)
	}
	return out, rows.Err()
}
