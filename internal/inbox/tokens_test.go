package inbox

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// lastUsedAt is written on use, at most once a minute per token, and starts over for a token made again.
func TestTokenLastUsedThrottle(t *testing.T) {
	s, clk := newStore(t, nil)
	ctx := context.Background()
	if _, err := s.CreateToken(ctx, "app", []string{"status"}, []string{"*"}, ""); err != nil {
		t.Fatal(err)
	}
	lastUsed := func() *time.Time {
		t.Helper()
		toks, err := s.Tokens(ctx)
		if err != nil || len(toks) != 1 {
			t.Fatalf("tokens = %v, %v", toks, err)
		}
		return toks[0].LastUsedAt
	}
	if lastUsed() != nil {
		t.Fatal("a token never used has lastUsedAt")
	}
	t0 := clk.now()
	use := func() {
		t.Helper()
		if err := s.TokenUsed(ctx, "app"); err != nil {
			t.Fatal(err)
		}
	}
	use()
	if got := lastUsed(); got == nil || !got.Equal(t0.Truncate(time.Millisecond)) {
		t.Fatalf("lastUsedAt = %v, want %v", got, t0)
	}
	clk.add(59 * time.Second)
	use()
	if got := lastUsed(); !got.Equal(t0.Truncate(time.Millisecond)) {
		t.Fatalf("written again within a minute: %v", got)
	}
	clk.add(2 * time.Second)
	use()
	if got := lastUsed(); !got.Equal(clk.now().Truncate(time.Millisecond)) {
		t.Fatalf("lastUsedAt = %v after a minute, want %v", got, clk.now())
	}

	if err := s.DeleteToken(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken(ctx, "app", []string{"status"}, []string{"*"}, ""); err != nil {
		t.Fatal(err)
	}
	use()
	if lastUsed() == nil {
		t.Fatal("a token made again under the same name was throttled by the old one")
	}
}

// A fleet.db from before prefix tokens and lastUsedAt opens, and its tokens read back without lastUsedAt.
func TestTokensTableMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE tokens (name TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, actions TEXT NOT NULL, computers TEXT NOT NULL, created_at INTEGER NOT NULL);
		INSERT INTO tokens VALUES ('old', 'h', '["status"]', '["*"]', 1700000000000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	toks, err := s.Tokens(context.Background())
	if err != nil || len(toks) != 1 || toks[0].Name != "old" || toks[0].LastUsedAt != nil || toks[0].Prefix != "" {
		t.Fatalf("tokens = %+v, %v", toks, err)
	}
	if err := s.TokenUsed(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
}
