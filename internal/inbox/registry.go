package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// The mTLS listener accepts a certificate only while its serial is the live
// one, so an entry is replaced on every start and deleted before the stop is
// issued. Grants are the snapshot taken at start, not the current Fleet.
const registrySchema = `
CREATE TABLE IF NOT EXISTS cert_registry (
  principal TEXT PRIMARY KEY,
  generation INTEGER NOT NULL,
  serial TEXT NOT NULL,
  grants TEXT NOT NULL DEFAULT '[]',
  at INTEGER NOT NULL
);
`

type CertEntry struct {
	Principal  string
	Generation int64
	Serial     string
	Send       []string
}

func (s *Store) RegisterCert(ctx context.Context, e CertEntry) error {
	g, _ := json.Marshal(e.Send)
	_, err := s.db.ExecContext(ctx, `INSERT INTO cert_registry (principal, generation, serial, grants, at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (principal) DO UPDATE SET generation = excluded.generation, serial = excluded.serial, grants = excluded.grants, at = excluded.at`,
		e.Principal, e.Generation, e.Serial, string(g), ms(s.now()))
	return err
}

func (s *Store) UnregisterCert(ctx context.Context, principal string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cert_registry WHERE principal = ?`, principal)
	return err
}

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

func (s *Store) LiveCerts(ctx context.Context) ([]CertEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT principal, generation, serial FROM cert_registry`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CertEntry
	for rows.Next() {
		var e CertEntry
		if err := rows.Scan(&e.Principal, &e.Generation, &e.Serial); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
