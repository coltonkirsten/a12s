package controller

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// stateFile is the Controller's only persistent record, in the state dir.
const stateFile = "state.json"

// State is what the Controller persists: the Fleet as written and the image IDs it pinned, and per
// Computer what it staged and what power the operator asked for.
// Credentials are never part of it.
type State struct {
	// Spec is the Fleet as written and Fleet its effective form, derived from Spec on load.
	Spec    *fleet.Spec  `json:"spec,omitempty"`
	Fleet   *fleet.Fleet `json:"-"`
	Version int64        `json:"version,omitempty"`
	// Pins keeps its JSON name from before v0.6.0, so an older Controller's state loads.
	Pins      *fleet.Pins        `json:"lock,omitempty"`
	Computers map[string]*Record `json:"computers"`
	// BundleHosts are the sign-in hosts of each pinned bundle, by
	// "kind@version", from its manifest.
	BundleHosts map[string][]string `json:"bundleHosts,omitempty"`
	// Services is each Service's persistent state, and Manifests the merged manifest each was
	// admitted with.
	Services  map[string]*ServiceRecord         `json:"services,omitempty"`
	Manifests map[string]*fleet.ServiceManifest `json:"manifests,omitempty"`

	// LegacyFleet and LegacySeccomp are read from a state file older than Spec, and migrated.
	LegacyFleet   json.RawMessage   `json:"fleet,omitempty"`
	LegacySeccomp map[string]string `json:"seccomp,omitempty"`
}

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
	// Send is grants.send and Uses the resolved endpoints of uses: what the mTLS listener and
	// the proxy allow.
	Send []string          `json:"send,omitempty"`
	Uses []driver.Endpoint `json:"uses,omitempty"`
	// Seccomp is the digest of the seccomp profile this generation runs with.
	Seccomp string `json:"seccomp,omitempty"`
}

func (r *Record) pending() bool { return r.StagedGeneration > r.Generation }

func loadState(path string) (*State, error) {
	st, err := readStateFile(path)
	if err != nil {
		return nil, err
	}
	if st.Spec == nil && len(st.LegacyFleet) > 0 {
		if st.Spec, err = migrateLegacy(filepath.Dir(path), st); err != nil {
			return nil, fmt.Errorf("load state %s: migrate: %w", path, err)
		}
	}
	st.LegacyFleet, st.LegacySeccomp = nil, nil
	if st.Spec != nil {
		// A Spec the Controller once admitted that no longer resolves would stop the whole resume: start empty
		// and let the next a12s up say why instead.
		if st.Fleet, err = st.Spec.Effective(); err != nil {
			return nil, fmt.Errorf("load state %s: %w", path, err)
		}
	}
	return st, nil
}

// migrateLegacy turns the effective Fleet an older Controller stored into a Spec: each agent computer as written is
// its merged form, its preset becomes files and its seccomp path the profile.
func migrateLegacy(stateDir string, st *State) (*fleet.Spec, error) {
	var old struct {
		APIVersion string                     `json:"apiVersion"`
		Kind       string                     `json:"kind"`
		Metadata   fleet.Metadata             `json:"metadata"`
		Egress     fleet.FleetEgress          `json:"egress"`
		Drives     map[string]fleet.Drive     `json:"drives"`
		Computers  map[string]json.RawMessage `json:"computers"`
		Services   map[string]fleet.Service   `json:"services"`
	}
	if err := json.Unmarshal(st.LegacyFleet, &old); err != nil {
		return nil, err
	}
	spec := &fleet.Spec{APIVersion: old.APIVersion, Kind: old.Kind, Metadata: old.Metadata, Egress: old.Egress, Drives: old.Drives,
		Services: old.Services, AgentComputers: map[string]json.RawMessage{}}
	lock := st.Pins
	if lock == nil {
		lock = fleet.NewPins()
	}
	for name, raw := range old.Computers {
		var c fleet.Computer
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("computers.%s: %w", name, err)
		}
		if p := c.Home.Preset; p != "" {
			files, err := filesFromTar(loadPreset(stateDir, lock.Presets[p]))
			if err != nil {
				return nil, fmt.Errorf("computers.%s.home.preset: %w", name, err)
			}
			c.Files, c.Home.Preset = files, ""
		}
		if p := c.Security.Seccomp.Path; p != "" {
			c.Security.Seccomp = fleet.Seccomp{Profile: json.RawMessage(st.LegacySeccomp[lock.Seccomp[p]])}
		}
		spec.AgentComputers[name] = fleet.SpecOf(c)
	}
	return spec, nil
}

// filesFromTar keeps a stored preset's text files; nil data is an empty preset.
func filesFromTar(data []byte) (map[string]string, error) {
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for len(data) > 0 {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if utf8.Valid(b) {
			out[hdr.Name] = string(b)
		}
	}
	return out, nil
}

func readStateFile(path string) (*State, error) {
	st := &State{Computers: map[string]*Record{}, BundleHosts: map[string][]string{}, Services: map[string]*ServiceRecord{}, Manifests: map[string]*fleet.ServiceManifest{}}
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
	if st.Services == nil {
		st.Services = map[string]*ServiceRecord{}
	}
	if st.Manifests == nil {
		st.Manifests = map[string]*fleet.ServiceManifest{}
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

// presetDir holds the preset tars of Controllers older than Spec, read once to migrate their state.
const presetDir = "presets"

func presetPath(stateDir, sum string) string {
	return filepath.Join(stateDir, presetDir, strings.TrimPrefix(sum, "sha256:")+".tar")
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
