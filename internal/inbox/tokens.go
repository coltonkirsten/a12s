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
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

const TokenPrefix = "a12st_"

func tokenHash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// CreateToken stores only the secret's hash; the returned secret is the only
// time it is ever visible.
func (s *Store) CreateToken(ctx context.Context, name string, actions, computers []string, prefix string) (*api.Token, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	secret := TokenPrefix + hex.EncodeToString(raw[:])
	a, _ := json.Marshal(actions)
	c, _ := json.Marshal(computers)
	now := s.now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens (name, hash, actions, computers, prefix, created_at) VALUES (?, ?, ?, ?, ?, ?)`, name, tokenHash(secret), string(a), string(c), prefix, ms(now))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errf(409, api.ErrConflict, "a12s token rm "+name+" first, or pick another name", "token %q already exists", name)
		}
		return nil, err
	}
	return &api.Token{Name: name, Actions: actions, Computers: computers, Prefix: prefix, CreatedAt: fromMS(ms(now)), Secret: secret}, nil
}

func (s *Store) LookupToken(ctx context.Context, secret string) (*api.Token, error) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return nil, nil
	}
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE hash = ?`, tokenHash(secret)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

const tokenCols = `name, actions, computers, prefix, created_at, last_used_at`

func scanToken(row interface{ Scan(...any) error }) (*api.Token, error) {
	var t api.Token
	var a, c string
	var created int64
	var used sql.NullInt64
	if err := row.Scan(&t.Name, &a, &c, &t.Prefix, &created, &used); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(a), &t.Actions)
	_ = json.Unmarshal([]byte(c), &t.Computers)
	t.CreatedAt = fromMS(created)
	if used.Valid {
		at := fromMS(used.Int64)
		t.LastUsedAt = &at
	}
	return &t, nil
}

// TokenUsed records that a token authenticated, writing at most once a minute per token.
func (s *Store) TokenUsed(ctx context.Context, name string) error {
	now := s.now()
	s.usedMu.Lock()
	if last, ok := s.used[name]; ok && now.Sub(last) < time.Minute {
		s.usedMu.Unlock()
		return nil
	}
	s.used[name] = now
	s.usedMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE name = ?`, ms(now), name)
	return err
}

func (s *Store) Tokens(ctx context.Context) ([]api.Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM tokens ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
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
	s.usedMu.Lock()
	delete(s.used, name)
	s.usedMu.Unlock()
	return nil
}

// Allows takes computer "" for Fleet-wide actions such as status.
func Allows(t *api.Token, action, computer string) bool {
	if t.Prefix != "" {
		// A prefix token may do everything inside its prefix; Fleet-wide reads are filtered by the caller.
		return computer == "" || strings.HasPrefix(computer, t.Prefix)
	}
	if !slices.Contains(t.Actions, action) {
		return false
	}
	return computer == "" || slices.Contains(t.Computers, "*") || slices.Contains(t.Computers, computer)
}
