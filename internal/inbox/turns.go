package inbox

import (
	"context"
	"encoding/json"

	"github.com/coltonkirsten/a12s/internal/api"
)

// KeepTurns is how many finished Turns per agent computer fleet.db keeps.
const KeepTurns = 200

const turnsSchema = `
CREATE TABLE IF NOT EXISTS turns (
  computer TEXT NOT NULL,
  id TEXT NOT NULL,
  finished_at INTEGER NOT NULL,
  data TEXT NOT NULL,
  PRIMARY KEY (computer, id)
);
CREATE INDEX IF NOT EXISTS turns_recent ON turns (computer, finished_at);
`

// SaveTurn records a finished Turn and drops the oldest beyond KeepTurns.
func (s *Store) SaveTurn(ctx context.Context, t api.Turn) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO turns (computer, id, finished_at, data) VALUES (?, ?, ?, ?)`, t.AgentComputer, t.ID, ms(t.FinishedAt), string(data)); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM turns WHERE computer = ? AND id NOT IN (SELECT id FROM turns WHERE computer = ? ORDER BY finished_at DESC, id DESC LIMIT ?)`, t.AgentComputer, t.AgentComputer, KeepTurns)
	return err
}

// Turns are the agent computer's last finished Turns, newest first.
func (s *Store) Turns(ctx context.Context, computer string, limit int) ([]api.Turn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM turns WHERE computer = ? ORDER BY finished_at DESC, id DESC LIMIT ?`, computer, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Turn{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var t api.Turn
		if err := json.Unmarshal([]byte(data), &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTurns(ctx context.Context, computer string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM turns WHERE computer = ?`, computer)
	return err
}
