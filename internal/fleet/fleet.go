// Package fleet parses, defaults, validates and diffs a12s Fleet files, and
// reads and writes fleet.lock.
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

const (
	AutonomyFull       = "full"
	AutonomyRestricted = "restricted"
)

const (
	AccessExclusive   = "exclusive"
	AccessShared      = "shared"
	AccessSharedWrite = "shared-write"
)

const (
	EnforcementEnforced   = "enforced"
	EnforcementUnenforced = "unenforced"
)

// AllowAll is the egress host that permits any public host on any port.
const AllowAll = "*"

// Fleet has defaults already applied to every Computer.
type Fleet struct {
	APIVersion string              `yaml:"apiVersion" json:"apiVersion"`
	Kind       string              `yaml:"kind" json:"kind"`
	Metadata   Metadata            `yaml:"metadata" json:"metadata"`
	Egress     FleetEgress         `yaml:"egress,omitempty" json:"egress,omitzero"`
	Defaults   Computer            `yaml:"defaults,omitempty" json:"defaults,omitzero"`
	Drives     map[string]Drive    `yaml:"drives,omitempty" json:"drives,omitempty"`
	Computers  map[string]Computer `yaml:"computers" json:"computers"`
	Services   map[string]Service  `yaml:"services,omitempty" json:"services,omitempty"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

type FleetEgress struct {
	// Enforcement is "" (enforced), "enforced" or "unenforced".
	Enforcement string `yaml:"enforcement,omitempty" json:"enforcement,omitempty"`
	// AllowPrivate lets Services' egress.tcp reach these private ranges; it never
	// covers the Fleet's own pools, loopback, link-local or metadata addresses.
	AllowPrivate []string `yaml:"allowPrivate,omitempty" json:"allowPrivate,omitempty"`
}

func (e FleetEgress) Unenforced() bool { return e.Enforcement == EnforcementUnenforced }

type Drive struct {
	Size   Bytes  `yaml:"size,omitempty" json:"size,omitzero"`
	Access string `yaml:"access" json:"access"`
}

// Computer has the Fleet defaults already merged in.
type Computer struct {
	Harness     Harness      `yaml:"harness,omitempty" json:"harness,omitzero"`
	Image       string       `yaml:"image,omitempty" json:"image,omitempty"`
	Resources   Resources    `yaml:"resources,omitempty" json:"resources,omitzero"`
	Home        Home         `yaml:"home,omitempty" json:"home,omitzero"`
	Autonomy    string       `yaml:"autonomy,omitempty" json:"autonomy,omitempty"`
	Credentials []string     `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	Drives      []DriveMount `yaml:"drives,omitempty" json:"drives,omitempty"`
	Egress      Egress       `yaml:"egress,omitempty" json:"egress,omitzero"`
	Security    Security     `yaml:"security,omitempty" json:"security,omitzero"`
	// Inbox is Controller state, not container config: never hashed into a generation.
	Inbox Inbox `yaml:"inbox,omitempty" json:"inbox,omitzero"`
	// Power is Controller state, never hashed into a generation. Off unless set.
	Power  Power  `yaml:"power,omitempty" json:"power,omitzero"`
	Uses   []Use  `yaml:"uses,omitempty" json:"uses,omitempty"`
	Grants Grants `yaml:"grants,omitempty" json:"grants,omitzero"`
}

type Power struct {
	// SleepAfterIdle is "off" or a duration from 1m to 7d ("15m", "2h", "1d").
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
	// Intro is the Batch intro; "" renders none, nil the default.
	Intro *string `yaml:"intro,omitempty" json:"intro,omitempty"`
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
	// Seccomp is a seccomp profile (Docker's JSON format), relative to the Fleet file, that replaces the substrate's
	// default profile. Capabilities and no-new-privileges are unchanged by it.
	Seccomp string `yaml:"seccomp,omitempty" json:"seccomp,omitempty"`
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

type Home struct {
	Size Bytes `yaml:"size,omitempty" json:"size,omitzero"`
	// Preset is a directory, relative to the Fleet file, copied once onto an empty home.
	Preset string `yaml:"preset,omitempty" json:"preset,omitempty"`
}

type DriveMount struct {
	Name     string `yaml:"name" json:"name"`
	At       string `yaml:"at" json:"at"`
	ReadOnly bool   `yaml:"readOnly,omitempty" json:"readOnly,omitempty"`
}

type Egress struct {
	Allow []string `yaml:"allow" json:"allow"`
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

func (f *Fleet) DriveNames() []string { return sortedKeys(f.Drives) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
