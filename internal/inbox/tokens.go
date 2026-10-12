package inbox

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
)

const TokenPrefix = "a12st_"

func tokenHash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// CreateToken stores only the secret's hash; the returned secret is the only
// time it is ever visible.
func (s *Store) CreateToken(ctx context.Context, name string, actions, computers []string) (*api.Token, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	secret := TokenPrefix + hex.EncodeToString(raw[:])
	a, _ := json.Marshal(actions)
	c, _ := json.Marshal(computers)
	now := s.now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens (name, hash, actions, computers, created_at) VALUES (?, ?, ?, ?, ?)`, name, tokenHash(secret), string(a), string(c), ms(now))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errf(409, api.ErrConflict, "a12s token rm "+name+" first, or pick another name", "token %q already exists", name)
		}
		return nil, err
	}
	return &api.Token{Name: name, Actions: actions, Computers: computers, CreatedAt: fromMS(ms(now)), Secret: secret}, nil
}

func (s *Store) LookupToken(ctx context.Context, secret string) (*api.Token, error) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return nil, nil
	}
	var t api.Token
	var a, c string
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT name, actions, computers, created_at FROM tokens WHERE hash = ?`, tokenHash(secret)).Scan(&t.Name, &a, &c, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(a), &t.Actions)
	_ = json.Unmarshal([]byte(c), &t.Computers)
	t.CreatedAt = fromMS(created)
	return &t, nil
}

func (s *Store) Tokens(ctx context.Context) ([]api.Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, actions, computers, created_at FROM tokens ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Token{}
	for rows.Next() {
		var t api.Token
		var a, c string
		var created int64
		if err := rows.Scan(&t.Name, &a, &c, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(a), &t.Actions)
		_ = json.Unmarshal([]byte(c), &t.Computers)
		t.CreatedAt = fromMS(created)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errf(404, api.ErrNotFound, "a12s token ls", "no token %q", name)
	}
	return nil
}

// Allows takes computer "" for Fleet-wide actions such as status.
func Allows(t *api.Token, action, computer string) bool {
	if !slices.Contains(t.Actions, action) {
		return false
	}
	return computer == "" || slices.Contains(t.Computers, "*") || slices.Contains(t.Computers, computer)
}
