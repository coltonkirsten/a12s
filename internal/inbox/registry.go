package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// registrySchema is the active-certificate registry (enhancements §5.3): one
// live entry per Computer or Service, written when the container starts,
// replaced with a fresh serial on every start and deleted before the stop is
// issued. The mTLS listener accepts a certificate only while its serial is
// the live one, and evaluates grants against the snapshot taken at start
// (the active generation's).
const registrySchema = `
CREATE TABLE IF NOT EXISTS cert_registry (
  principal TEXT PRIMARY KEY,
  generation INTEGER NOT NULL,
  serial TEXT NOT NULL,
  grants TEXT NOT NULL DEFAULT '[]',
  at INTEGER NOT NULL
);
`

// CertEntry is a live certificate.
type CertEntry struct {
	Principal  string
	Generation int64
	Serial     string
	Send       []string
}

// RegisterCert makes serial the live certificate of principal, replacing any
// earlier one.
func (s *Store) RegisterCert(ctx context.Context, e CertEntry) error {
	g, _ := json.Marshal(e.Send)
	_, err := s.db.ExecContext(ctx, `INSERT INTO cert_registry (principal, generation, serial, grants, at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (principal) DO UPDATE SET generation = excluded.generation, serial = excluded.serial, grants = excluded.grants, at = excluded.at`,
		e.Principal, e.Generation, e.Serial, string(g), ms(s.now()))
	return err
}

// UnregisterCert deletes principal's live certificate: from now on it is refused.
func (s *Store) UnregisterCert(ctx context.Context, principal string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cert_registry WHERE principal = ?`, principal)
	return err
}

// LiveCert returns principal's live certificate, if any.
func (s *Store) LiveCert(ctx context.Context, principal string) (CertEntry, bool, error) {
	e := CertEntry{Principal: principal}
	var g string
	err := s.db.QueryRowContext(ctx, `SELECT generation, serial, grants FROM cert_registry WHERE principal = ?`, principal).Scan(&e.Generation, &e.Serial, &g)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, err
	}
	_ = json.Unmarshal([]byte(g), &e.Send)
	return e, true, nil
}
