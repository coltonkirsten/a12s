// Package fleet parses, defaults, validates and diffs a12s Fleet files (§3),
// and reads and writes fleet.lock.
package fleet

import "sort"

// APIVersion and Kind identify a Fleet document.
const (
	APIVersion = "a12s.ai/v1alpha1"
	Kind       = "Fleet"
)

// Harness kinds supported in Layer 1.
const (
	HarnessClaudeCode = "claude-code"
	HarnessCodex      = "codex"
)

// Image tiers. Any other image value is a custom image reference.
const (
	TierSlim    = "slim"
	TierBrowser = "browser"
	TierCustom  = "custom"
)

// Autonomy levels (§5 "Permission prompts").
const (
	AutonomyFull       = "full"
	AutonomyRestricted = "restricted"
)

// Drive access modes (§4 "Drive access").
const (
	AccessExclusive   = "exclusive"
	AccessShared      = "shared"
	AccessSharedWrite = "shared-write"
)

// Fleet-level egress enforcement values (§6).
const (
	EnforcementEnforced   = "enforced"
	EnforcementUnenforced = "unenforced"
)

// AllowAll is the egress host that permits any public host on any port.
const AllowAll = "*"

// Fleet is a parsed Fleet with defaults applied to every Computer.
type Fleet struct {
	APIVersion string              `yaml:"apiVersion" json:"apiVersion"`
	Kind       string              `yaml:"kind" json:"kind"`
	Metadata   Metadata            `yaml:"metadata" json:"metadata"`
	Egress     FleetEgress         `yaml:"egress,omitempty" json:"egress,omitzero"`
	Defaults   Computer            `yaml:"defaults,omitempty" json:"defaults,omitzero"`
	Drives     map[string]Drive    `yaml:"drives,omitempty" json:"drives,omitempty"`
	Computers  map[string]Computer `yaml:"computers" json:"computers"`
	// Services are non-agent containers (enhancements §5).
	Services map[string]Service `yaml:"services,omitempty" json:"services,omitempty"`
}

// Metadata names the Fleet.
type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

// FleetEgress holds Fleet-wide egress settings.
type FleetEgress struct {
	// Enforcement is "" (enforced), "enforced" or "unenforced".
	Enforcement string `yaml:"enforcement,omitempty" json:"enforcement,omitempty"`
	// AllowPrivate lets Services' egress.tcp reach these private ranges
	// (enhancements §5.4). Loud; never covers the Fleet's own pools,
	// loopback, link-local or metadata addresses.
	AllowPrivate []string `yaml:"allowPrivate,omitempty" json:"allowPrivate,omitempty"`
}

// Unenforced reports whether the Fleet accepts unenforced egress.
func (e FleetEgress) Unenforced() bool { return e.Enforcement == EnforcementUnenforced }

// Drive declares a work drive shared by name across the Fleet.
type Drive struct {
	Size   Bytes  `yaml:"size,omitempty" json:"size,omitzero"`
	Access string `yaml:"access" json:"access"`
}

// Computer is one Computer's resolved spec (defaults already merged in).
type Computer struct {
	Harness     Harness      `yaml:"harness,omitempty" json:"harness,omitzero"`
	Image       string       `yaml:"image,omitempty" json:"image,omitempty"`
	Resources   Resources    `yaml:"resources,omitempty" json:"resources,omitzero"`
	Home        Home         `yaml:"home,omitempty" json:"home,omitzero"`
	Autonomy    string       `yaml:"autonomy,omitempty" json:"autonomy,omitempty"`
	Credentials []string     `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	Drives      []DriveMount `yaml:"drives,omitempty" json:"drives,omitempty"`
	Egress      Egress       `yaml:"egress,omitempty" json:"egress,omitzero"`
	// Inbox configures the Computer's inbox (enhancements §4.8, §4.13).
	// Controller state, not container config: never hashed into a generation.
	Inbox Inbox `yaml:"inbox,omitempty" json:"inbox,omitzero"`
	// Power is the declared power policy (enhancements §4.14): Controller
	// state, never hashed into a generation. Off unless set.
	Power Power `yaml:"power,omitempty" json:"power,omitzero"`
	// Uses lists the Services this Computer may reach (enhancements §5.4).
	Uses []Use `yaml:"uses,omitempty" json:"uses,omitempty"`
	// Grants are this Computer's capabilities: send to other inboxes (§5.3).
	Grants Grants `yaml:"grants,omitempty" json:"grants,omitzero"`
}

// Power is a Computer's power policy.
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

// Inbox is a Computer's inbox configuration. Unset fields fall back to
// defaults.inbox, then the built-in values.
type Inbox struct {
	MaxMessages *int64 `yaml:"maxMessages,omitempty" json:"maxMessages,omitempty"`
	// MaxBytes is a size as written ("320Ki"); validated at admission.
	MaxBytes *string `yaml:"maxBytes,omitempty" json:"maxBytes,omitempty"`
	// Intro is the Batch intro; "" renders none, nil the default.
	Intro *string `yaml:"intro,omitempty" json:"intro,omitempty"`
}

// Tier returns the image tier: slim, browser or custom.
func (c Computer) Tier() string {
	switch c.Image {
	case TierSlim, TierBrowser:
		return c.Image
	}
	return TierCustom
}

// Harness selects the harness program and, optionally, the model.
type Harness struct {
	Kind    string `yaml:"kind,omitempty" json:"kind,omitempty"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	Model   string `yaml:"model,omitempty" json:"model,omitempty"`
}

// BundleKey returns the fleet.lock key for this harness: "kind@version".
func (h Harness) BundleKey() string { return BundleKey(h.Kind, h.Version) }

// Resources are per-Computer limits. A zero value means no limit.
type Resources struct {
	CPU    CPU   `yaml:"cpu,omitempty" json:"cpu,omitzero"`
	Memory Bytes `yaml:"memory,omitempty" json:"memory,omitzero"`
}

// Home describes the Computer's home drive.
type Home struct {
	Size Bytes `yaml:"size,omitempty" json:"size,omitzero"`
	// Preset is a directory, relative to the Fleet file, copied once onto an empty home.
	Preset string `yaml:"preset,omitempty" json:"preset,omitempty"`
}

// DriveMount attaches a Fleet drive to a Computer.
type DriveMount struct {
	Name     string `yaml:"name" json:"name"`
	At       string `yaml:"at" json:"at"`
	ReadOnly bool   `yaml:"readOnly,omitempty" json:"readOnly,omitempty"`
}

// Egress lists the hosts a Computer may reach through the egress proxy.
type Egress struct {
	Allow []string `yaml:"allow" json:"allow"`
}

// AllowsAll reports whether the allowlist contains "*".
func (e Egress) AllowsAll() bool {
	for _, h := range e.Allow {
		if h == AllowAll {
			return true
		}
	}
	return false
}

// ComputerNames returns the Computer names in sorted order.
func (f *Fleet) ComputerNames() []string { return sortedKeys(f.Computers) }

// DriveNames returns the drive names in sorted order.
func (f *Fleet) DriveNames() []string { return sortedKeys(f.Drives) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
