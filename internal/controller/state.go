package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
	// BundleHosts are the sign-in hosts of each pinned bundle, by
	// "kind@version", from its manifest.
	BundleHosts map[string][]string `json:"bundleHosts,omitempty"`
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
	// Session and ResumePending are computerd's last report (shown asleep).
	Session       string `json:"session,omitempty"`
	ResumePending bool   `json:"resumePending,omitempty"`
	// Active is the configuration of Generation that the Controller itself
	// enforces: it changes when a generation is staged on the substrate,
	// not when a Fleet is merely accepted.
	Active Active `json:"active"`
}

// Active is the part of a generation's spec enforced outside the substrate.
type Active struct {
	// Allow is egress.allow and SignInHosts the bundle's sign-in hosts:
	// together the egress proxy's policy.
	Allow       []string `json:"allow,omitempty"`
	SignInHosts []string `json:"signInHosts,omitempty"`
	// Drives are the attachments, which authorize leases.
	Drives []fleet.DriveMount `json:"drives,omitempty"`
	// Unenforced is the Fleet's egress.enforcement: unenforced when this
	// generation was staged.
	Unenforced bool `json:"unenforced,omitempty"`
}

// pending reports whether a staged change waits for idle.
func (r *Record) pending() bool { return r.StagedGeneration > r.Generation }

func loadState(path string) (*State, error) {
	st := &State{Computers: map[string]*Record{}, BundleHosts: map[string][]string{}}
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
	if st.BundleHosts == nil {
		st.BundleHosts = map[string][]string{}
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

// presetDir holds preset tars by content hash. Presets are not secrets; a
// restarted Controller needs them to stage a Computer onto an empty home.
const presetDir = "presets"

func presetPath(stateDir, sum string) string {
	return filepath.Join(stateDir, presetDir, strings.TrimPrefix(sum, "sha256:")+".tar")
}

// savePreset stores a preset tar whose content hash was verified as sum.
// A cached entry is kept only if it still verifies; a corrupted one is
// replaced atomically by the upload.
func savePreset(stateDir, sum string, data []byte) error {
	p := presetPath(stateDir, sum)
	if loadPreset(stateDir, sum) != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("save preset: %w", err)
	}
	if err := writeFileAtomic(p, data, 0o600); err != nil {
		return fmt.Errorf("save preset: %w", err)
	}
	return nil
}

// loadPreset returns the stored tar for sum, or nil when it is missing or
// no longer matches its hash.
func loadPreset(stateDir, sum string) []byte {
	data, err := os.ReadFile(presetPath(stateDir, sum))
	if err != nil {
		return nil
	}
	if got, err := hashPresetTar(data); err != nil || got != sum {
		return nil
	}
	return data
}

// prunePresets removes stored presets the lock no longer pins.
func prunePresets(stateDir string, keep map[string]string) {
	want := map[string]bool{}
	for _, sum := range keep {
		want[filepath.Base(presetPath(stateDir, sum))] = true
	}
	entries, _ := os.ReadDir(filepath.Join(stateDir, presetDir))
	for _, e := range entries {
		if !want[e.Name()] {
			_ = os.Remove(filepath.Join(stateDir, presetDir, e.Name()))
		}
	}
}
