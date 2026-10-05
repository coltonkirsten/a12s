package driver

import (
	"strconv"
	"strings"
)

// DriveKind distinguishes a Computer's home drive from Fleet work drives.
type DriveKind string

const (
	DriveHome DriveKind = "home"
	DriveWork DriveKind = "work"
)

// HomeDriveID is the drive ID of a Computer's home drive: "home-<computer>".
func HomeDriveID(computer string) string { return "home-" + computer }

// WorkDriveID is the drive ID of a Fleet work drive: "drive-<name>".
func WorkDriveID(drive string) string { return "drive-" + drive }

// DriveSpec declares a drive.
type DriveSpec struct {
	ID   string    `json:"id"` // HomeDriveID or WorkDriveID
	Kind DriveKind `json:"kind"`
	// Computer owns a home drive; empty for work drives.
	Computer string `json:"computer,omitempty"`
	// Size in bytes; recorded, and enforced only where Capabilities.DriveQuota.
	Size int64 `json:"size,omitempty"`
}

// Mount attaches a drive inside a Computer.
type Mount struct {
	DriveID  string `json:"driveId"`
	At       string `json:"at"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// BundleRef identifies the HarnessBundle a Computer runs.
type BundleRef struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Image   string `json:"image"`
}

// BundleRoot is where the bundle cache is mounted read-only in Computers.
const BundleRoot = "/opt/a12s/bundles"

// BundleDir is the cache directory name of a bundle: "<kind>-<version>".
func BundleDir(kind, version string) string { return kind + "-" + version }

// BundlePath is the bundle's path inside a Computer.
func BundlePath(kind, version string) string { return BundleRoot + "/" + BundleDir(kind, version) }

// BundleManifest is /bundle/bundle.json in a HarnessBundle image.
type BundleManifest struct {
	Kind        string   `json:"kind"`
	Version     string   `json:"version"`
	Arch        string   `json:"arch"`
	Bin         string   `json:"bin"`
	SignInHosts []string `json:"signInHosts"`
}

// Bundle is the result of EnsureBundle.
type Bundle struct {
	ImageID  string         `json:"imageId"`
	Manifest BundleManifest `json:"manifest"`
}

// ComputerSpec is everything a Driver needs to stage one generation.
type ComputerSpec struct {
	Name       string `json:"name"`
	Generation int64  `json:"generation"`
	Image      string `json:"image"` // image reference or ID to run
	// Tier is "slim", "browser" or "custom".
	Tier      string    `json:"tier"`
	Bundle    BundleRef `json:"bundle"`
	Model     string    `json:"model,omitempty"`
	Autonomy  string    `json:"autonomy"`
	CPUMillis int64     `json:"cpuMillis,omitempty"`   // 0 = no limit
	Memory    int64     `json:"memoryBytes,omitempty"` // 0 = no limit
	HomeDrive string    `json:"homeDrive"`             // HomeDriveID(Name)
	Drives    []Mount   `json:"drives,omitempty"`
	// Credentials are the names (not values) the Computer expects.
	Credentials []string `json:"credentials,omitempty"`
	// ProxyURL and ControllerURL default to the Fleet's controller.
	ProxyURL      string `json:"proxyUrl,omitempty"`
	ControllerURL string `json:"controllerUrl,omitempty"`
	// Preset is a tar stream (entries relative to the agent's home) applied
	// once, before the first start on an empty home drive. Optional.
	Preset []byte `json:"preset,omitempty"`
	// Seccomp is a seccomp profile (JSON) to run under instead of the
	// substrate default; the browser tier ships one. Optional.
	Seccomp string `json:"seccomp,omitempty"`
}

// Env returns the environment every Computer gets (PLAN "Container").
// fleet names the Fleet; proxy and controller URLs must already be resolved.
func (s ComputerSpec) Env(fleet string) []string {
	env := []string{
		"A12S_COMPUTER=" + s.Name,
		"A12S_FLEET=" + fleet,
		"A12S_GENERATION=" + strconv.FormatInt(s.Generation, 10),
		"A12S_HARNESS_KIND=" + s.Bundle.Kind,
		"A12S_HARNESS_VERSION=" + s.Bundle.Version,
		"A12S_AUTONOMY=" + s.Autonomy,
		"A12S_IMAGE_TIER=" + s.Tier,
		"A12S_BUNDLE=" + BundlePath(s.Bundle.Kind, s.Bundle.Version),
		"A12S_CREDENTIALS=" + strings.Join(s.Credentials, ","),
		"A12S_PROXY=" + s.ProxyURL,
		"A12S_CONTROLLER=" + s.ControllerURL,
	}
	if s.Model != "" {
		env = append(env, "A12S_MODEL="+s.Model)
	}
	return env
}
