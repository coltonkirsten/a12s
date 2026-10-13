package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

type DriveKind string

const (
	DriveHome DriveKind = "home"
	// DriveKeep holds one kept path of an attached folder.
	DriveKeep DriveKind = "keep"
)

func HomeDriveID(computer string) string { return "home-" + computer }

const keepDrivePrefix = "keep-"

// KeepDriveID names the drive for one kept path. The digest keeps IDs apart when names and paths share hyphens
// ("a-b" + "c" and "a" + "b-c"), and the rest keeps it readable.
func KeepDriveID(computer, attach, keep string) string {
	sum := sha256.Sum256([]byte(computer + "\x00" + attach + "\x00" + keep))
	slug := strings.NewReplacer("/", "_", ".", "_").Replace(strings.TrimLeft(keep, "."))
	return keepDrivePrefix + computer + "-" + attach + "-" + slug + "-" + hex.EncodeToString(sum[:4])
}

func IsKeepDrive(id string) bool { return strings.HasPrefix(id, keepDrivePrefix) }

type DriveSpec struct {
	ID   string    `json:"id"` // HomeDriveID or StateDriveID
	Kind DriveKind `json:"kind"`
	// Computer is the owning Computer or Service.
	Computer string `json:"computer,omitempty"`
	// Path is where a keep drive is mounted.
	Path string `json:"path,omitempty"`
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
	// Attach is sorted by name.
	Attach []Attach `json:"attach,omitempty"`
}

// Attach is a host folder bound at Target, with each kept path on a drive of its own.
type Attach struct {
	Host     string `json:"host"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	Keep     []Keep `json:"keep,omitempty"`
}

type Keep struct {
	Drive  string `json:"drive"`
	Target string `json:"target"`
}

// AttachTargets are every attached folder's mount point, for computerd to check before it writes anything.
func (s ComputerSpec) AttachTargets() []string {
	var out []string
	for _, a := range s.Attach {
		out = append(out, a.Target)
	}
	return out
}

// KeepTargets are every kept path's mount point, for computerd to hand to the agent's uid.
func (s ComputerSpec) KeepTargets() []string {
	var out []string
	for _, a := range s.Attach {
		for _, k := range a.Keep {
			out = append(out, k.Target)
		}
	}
	return out
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
	if attach := s.AttachTargets(); len(attach) > 0 {
		env = append(env, "A12S_ATTACH="+strings.Join(attach, ","))
	}
	if keep := s.KeepTargets(); len(keep) > 0 {
		env = append(env, "A12S_KEEP="+strings.Join(keep, ","))
	}
	return env
}
