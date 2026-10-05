package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// stateFile is the Controller's only persistent record, in the state dir.
const stateFile = "state.json"

// State is what the Controller persists: the applied Fleet and Lock, and per
// Computer what it staged and what power the operator asked for.
// Credentials are never part of it.
type State struct {
	Fleet     *fleet.Fleet       `json:"fleet,omitempty"`
	Lock      *fleet.Lock        `json:"lock,omitempty"`
	Computers map[string]*Record `json:"computers"`
}

// Record is one Computer's persistent state.
type Record struct {
	// Generation is the generation staged on the substrate, and SpecHash
	// the hash of its spec.
	Generation int64  `json:"generation"`
	SpecHash   string `json:"specHash,omitempty"`
	// DesiredPower is the operator's choice: set by `up --asleep` (new
	// Computers only), `sleep` and `wake`.
	DesiredPower driver.Power `json:"desiredPower"`
	// StagedGeneration is greater than Generation while a change waits for
	// the Computer's next idle boundary; StagedHash is its spec hash.
	StagedGeneration int64  `json:"stagedGeneration,omitempty"`
	StagedHash       string `json:"stagedHash,omitempty"`
	// Restarts counts restarts after unexpected deaths since the last
	// operator action.
	Restarts int `json:"restarts,omitempty"`
	// CredentialGeneration is the last value written to .generation.
	CredentialGeneration int64 `json:"credentialGeneration,omitempty"`
	// Session is the last session id computerd reported (shown while asleep).
	Session string `json:"session,omitempty"`
	// SignInHosts come from the bundle's manifest and join the egress allowlist.
	SignInHosts []string `json:"signInHosts,omitempty"`
}

// pending reports whether a staged change waits for idle.
func (r *Record) pending() bool { return r.StagedGeneration > r.Generation }

func loadState(path string) (*State, error) {
	st := &State{Computers: map[string]*Record{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("load state %s: %w", path, err)
	}
	if st.Computers == nil {
		st.Computers = map[string]*Record{}
	}
	return st, nil
}

func (s *State) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if err := writeFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
