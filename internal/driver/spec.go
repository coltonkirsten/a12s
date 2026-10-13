package driver

import (
	"encoding/json"
	"strconv"
	"strings"
)

type DriveKind string

const DriveHome DriveKind = "home"

func HomeDriveID(computer string) string { return "home-" + computer }

type DriveSpec struct {
	ID   string    `json:"id"` // HomeDriveID or StateDriveID
	Kind DriveKind `json:"kind"`
	// Computer is the owning Computer or Service.
	Computer string `json:"computer,omitempty"`
	// Size is enforced only where Capabilities.DriveQuota.
	Size int64 `json:"size,omitempty"`
}

type BundleRef struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Image   string `json:"image"`
}

func (b BundleRef) Dir() string { return BundleDir(b.Kind, b.Version, b.Image) }

func (b BundleRef) Path() string { return BundlePath(b.Kind, b.Version, b.Image) }

const BundleRoot = "/opt/a12s/bundles"

// BundleDir includes the image ID so a different image claiming the same kind
// and version never reuses another image's files.
func BundleDir(kind, version, imageID string) string {
	return kind + "-" + version + "-" + ShortImageID(imageID)
}

func BundlePath(kind, version, imageID string) string {
	return BundleRoot + "/" + BundleDir(kind, version, imageID)
}

// ShortImageID returns "" if id is not an image ID.
func ShortImageID(id string) string {
	hex := strings.TrimPrefix(id, "sha256:")
	if len(hex) < 12 {
		return ""
	}
	for _, c := range hex {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return ""
		}
	}
	return hex[:12]
}

type BundleManifest struct {
	Kind        string   `json:"kind"`
	Version     string   `json:"version"`
	Arch        string   `json:"arch"`
	Bin         string   `json:"bin"`
	SignInHosts []string `json:"signInHosts"`
}

type Bundle struct {
	ImageID  string         `json:"imageId"`
	Manifest BundleManifest `json:"manifest"`
}

type ComputerSpec struct {
	Name       string    `json:"name"`
	Generation int64     `json:"generation"`
	Image      string    `json:"image"` // image reference or ID to run
	Tier       string    `json:"tier"`
	Bundle     BundleRef `json:"bundle"`
	Model      string    `json:"model,omitempty"`
	CPUMillis  int64     `json:"cpuMillis,omitempty"`   // 0 = no limit
	Memory     int64     `json:"memoryBytes,omitempty"` // 0 = no limit
	HomeDrive  string    `json:"homeDrive"`             // HomeDriveID(Name)
	// Credentials are names only, never values.
	Credentials   []string `json:"credentials,omitempty"`
	ProxyURL      string   `json:"proxyUrl,omitempty"`
	ControllerURL string   `json:"controllerUrl,omitempty"`
	// Files is a tar copied onto the home drive before the first start and again whenever FilesDigest changes.
	Files       []byte `json:"files,omitempty"`
	FilesDigest string `json:"filesDigest,omitempty"`
	// Seccomp is a profile from the Fleet's security.seccomp that replaces the substrate default.
	Seccomp string     `json:"seccomp,omitempty"`
	Uses    []Endpoint `json:"uses,omitempty"`
	Send    []string   `json:"send,omitempty"`
}

// Env requires proxy and controller URLs to be resolved already.
func (s ComputerSpec) Env(fleet string) []string {
	env := []string{
		"A12S_COMPUTER=" + s.Name,
		"A12S_FLEET=" + fleet,
		"A12S_GENERATION=" + strconv.FormatInt(s.Generation, 10),
		"A12S_HARNESS_KIND=" + s.Bundle.Kind,
		"A12S_HARNESS_VERSION=" + s.Bundle.Version,
		"A12S_IMAGE_TIER=" + s.Tier,
		"A12S_BUNDLE=" + s.Bundle.Path(),
		"A12S_CREDENTIALS=" + strings.Join(s.Credentials, ","),
		"A12S_PROXY=" + s.ProxyURL,
		"A12S_CONTROLLER=" + s.ControllerURL,
	}
	if s.Model != "" {
		env = append(env, "A12S_MODEL="+s.Model)
	}
	if len(s.Uses) > 0 {
		b, _ := json.Marshal(s.Uses)
		env = append(env, "A12S_USES="+string(b))
	}
	if len(s.Send) > 0 {
		env = append(env, "A12S_SEND="+strings.Join(s.Send, ","))
	}
	return env
}
