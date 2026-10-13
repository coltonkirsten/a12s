// Package fleet parses, defaults, validates and diffs a12s Fleet files, and
// holds the image pins the Controller keeps.
package fleet

import "sort"

const (
	APIVersion = "a12s.ai/v1alpha1"
	Kind       = "Fleet"
)

const (
	HarnessClaudeCode = "claude-code"
	HarnessCodex      = "codex"
)

// Any other image value is a custom image reference.
const (
	TierSlim   = "slim"
	TierCustom = "custom"
)

// AllowAll is the egress host that permits any public host on any port.
const AllowAll = "*"

// Fleet is the effective Fleet: defaults already applied to every agent computer. Spec is the Fleet as written.
type Fleet struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   Metadata            `json:"metadata"`
	Defaults   Computer            `json:"defaults,omitzero"`
	Computers  map[string]Computer `json:"agent_computers"`
	Services   map[string]Service  `json:"services,omitempty"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

// Computer is one agent computer's spec. In a Fleet it has the defaults merged in; in a Spec it is as written.
// Lists are omitzero, not omitempty, so an explicit empty list still replaces the defaults' one (credentials add to
// the defaults' list instead).
type Computer struct {
	Harness Harness `yaml:"harness,omitempty" json:"harness,omitzero"`
	Image   string  `yaml:"image,omitempty" json:"image,omitempty"`
	// Instructions become CLAUDE.md (claude-code) or AGENTS.md (codex) on the home drive.
	Instructions string `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	// Files are more text files for the home drive, by path relative to it.
	Files       map[string]string `yaml:"files,omitempty" json:"files,omitempty"`
	Resources   Resources         `yaml:"resources,omitempty" json:"resources,omitzero"`
	Credentials []string          `yaml:"credentials,omitempty" json:"credentials,omitzero"`
	Egress      Egress            `yaml:"egress,omitempty" json:"egress,omitzero"`
	Security    Security          `yaml:"security,omitempty" json:"security,omitzero"`
	// Inbox is Controller state, not container config: never hashed into a generation.
	Inbox Inbox `yaml:"inbox,omitempty" json:"inbox,omitzero"`
	// Power is Controller state, never hashed into a generation. Off unless set.
	Power  Power  `yaml:"power,omitempty" json:"power,omitzero"`
	Uses   []Use  `yaml:"uses,omitempty" json:"uses,omitzero"`
	Grants Grants `yaml:"grants,omitempty" json:"grants,omitzero"`
}

type Power struct {
	// SleepAfterIdle is "off" or a duration from 1s to 7d ("5s", "15m", "2h", "1d").
	SleepAfterIdle *string        `yaml:"sleepAfterIdle,omitempty" json:"sleepAfterIdle,omitempty"`
	WakeOnMessage  *WakeOnMessage `yaml:"wakeOnMessage,omitempty" json:"wakeOnMessage,omitempty"`
}

// WakeOnMessage lists the principals whose pending Messages may wake the
// Computer, rate-limited.
type WakeOnMessage struct {
	From            []string `yaml:"from" json:"from"`
	MaxWakesPerHour *int     `yaml:"maxWakesPerHour,omitempty" json:"maxWakesPerHour,omitempty"`
}

// Inbox's unset fields fall back to defaults.inbox, then the built-in values.
type Inbox struct {
	MaxMessages *int64 `yaml:"maxMessages,omitempty" json:"maxMessages,omitempty"`
	// MaxBytes is a size as written ("320Ki"); validated at admission.
	MaxBytes *string `yaml:"maxBytes,omitempty" json:"maxBytes,omitempty"`
}

func (c Computer) Tier() string {
	switch c.Image {
	case TierSlim:
		return c.Image
	}
	return TierCustom
}

// Security is set only by the Fleet's owner: nothing an image carries can loosen the sandbox.
type Security struct {
	// Seccomp replaces the substrate's default seccomp profile. Capabilities and no-new-privileges are unchanged by it.
	Seccomp Seccomp `yaml:"seccomp,omitempty" json:"seccomp,omitzero"`
}

type Harness struct {
	Kind    string `yaml:"kind,omitempty" json:"kind,omitempty"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	Model   string `yaml:"model,omitempty" json:"model,omitempty"`
}

func (h Harness) BundleKey() string { return BundleKey(h.Kind, h.Version) }

// A zero Resources value means no limit.
type Resources struct {
	CPU    CPU   `yaml:"cpu,omitempty" json:"cpu,omitzero"`
	Memory Bytes `yaml:"memory,omitempty" json:"memory,omitzero"`
}

type Egress struct {
	Allow []string `yaml:"allow" json:"allow,omitzero"`
}

func (e Egress) AllowsAll() bool {
	for _, h := range e.Allow {
		if h == AllowAll {
			return true
		}
	}
	return false
}

func (f *Fleet) ComputerNames() []string { return sortedKeys(f.Computers) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
