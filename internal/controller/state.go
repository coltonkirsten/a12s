package controller

import (
	"bytes"
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
}

type Record struct {
	// Generation is the generation staged on the substrate, and SpecHash
	// the hash of its spec.
	Generation int64  `json:"generation"`
	SpecHash   string `json:"specHash,omitempty"`
	// DesiredPower is the operator's choice: set by `sleep` and `wake`.
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
	if st.Spec != nil {
		// A Spec the Controller once admitted that no longer resolves would stop the whole resume: start empty
		// and let the next a12s up say why instead.
		if st.Fleet, err = st.Spec.Effective(); err != nil {
			return nil, fmt.Errorf("load state %s: %w", path, err)
		}
	}
	return st, nil
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
	if data, err = dropRemovedFields(data); err != nil {
		return nil, fmt.Errorf("load state %s: %w", path, err)
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

// dropRemovedFields strips the Fleet fields v0.7.0 removed from the stored Spec: the Spec decodes strictly, and a
// Controller that can't load its own state can't take the a12s up that would fix it.
func dropRemovedFields(data []byte) ([]byte, error) {
	var st map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&st); err != nil {
		return nil, err
	}
	spec, _ := st["spec"].(map[string]any)
	if spec == nil {
		return data, nil
	}
	delete(spec, "drives")
	delete(spec, "egress")
	computers := []any{spec["defaults"]}
	for _, c := range asObject(spec["agent_computers"]) {
		computers = append(computers, c)
	}
	for _, c := range computers {
		c := asObject(c)
		delete(c, "home")
		delete(c, "autonomy")
		delete(c, "drives")
		delete(asObject(c["inbox"]), "intro")
	}
	for _, s := range asObject(spec["services"]) {
		s := asObject(s)
		delete(s, "publish")
		delete(asObject(s["egress"]), "tcp")
		delete(asObject(s["manifest"]), "login")
	}
	return json.Marshal(st)
}

func asObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
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
