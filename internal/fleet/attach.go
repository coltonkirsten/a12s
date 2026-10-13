package fleet

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

// Attach is a host folder mounted into an agent computer's home at /home/agent/<name>.
type Attach struct {
	// Host is absolute everywhere but a Fleet file, where it may also be relative to the file or start with ~/.
	Host     string `yaml:"host" json:"host"`
	ReadOnly bool   `yaml:"readOnly,omitempty" json:"readOnly,omitempty"`
	// Keep lists paths under the mount that live on a drive of their own instead of in the host folder.
	Keep []string `yaml:"keep,omitempty" json:"keep,omitempty"`
}

const (
	MaxAttach = 8
	MaxKeep   = 16
	AgentHome = "/home/agent"
)

// reservedAttachNames are home entries an agent relies on that a mount would hide. Dotfiles (.claude, .codex) and
// CLAUDE.md or AGENTS.md can't be named: a name is a DNS label.
var reservedAttachNames = map[string]bool{"bin": true}

// socketDirs hold the Docker socket on common hosts; mounting one of them, or a folder that contains one, would hand
// the agent the Docker daemon.
var socketDirs = []string{"/run", "/var/run", "/private/var/run"}

// HostIsFileRelative reports whether host is written relative to the Fleet file (or to ~) and must be resolved
// before it reaches the Controller.
func HostIsFileRelative(host string) bool { return host != "" && !path.IsAbs(host) }

// AttachTarget is where an attached folder appears in the agent computer.
func AttachTarget(name string) string { return AgentHome + "/" + name }

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

func validateAttach(c Computer) []string {
	var bad []string
	if len(c.Attach) > MaxAttach {
		bad = append(bad, fmt.Sprintf("attach: %d folders, more than %d", len(c.Attach), MaxAttach))
	}
	homeTop := map[string]bool{}
	for p := range c.HomeFiles() {
		top, _, _ := strings.Cut(p, "/")
		homeTop[top] = true
	}
	for _, name := range slices.Sorted(maps.Keys(c.Attach)) {
		a := c.Attach[name]
		field := "attach." + name
		switch {
		case !nameRE.MatchString(name):
			bad = append(bad, fmt.Sprintf("%s: name must be a lowercase DNS label of at most 40 characters (it becomes /home/agent/<name>)", field))
		case reservedAttachNames[name]:
			bad = append(bad, fmt.Sprintf("%s: %q is reserved in the home directory; pick another name", field, name))
		case homeTop[name]:
			bad = append(bad, fmt.Sprintf("%s: files or instructions already write %s/ in the home directory; pick another name", field, name))
		}
		if msg := validateHost(a.Host); msg != "" {
			bad = append(bad, fmt.Sprintf("%s.host: %s", field, msg))
		}
		if len(a.Keep) > MaxKeep {
			bad = append(bad, fmt.Sprintf("%s.keep: %d paths, more than %d", field, len(a.Keep), MaxKeep))
		}
		if len(a.Keep) > 0 && a.ReadOnly {
			bad = append(bad, fmt.Sprintf("%s.keep: needs a writable folder (readOnly: false): a kept path is mounted inside it", field))
		}
		seen := map[string]bool{}
		for _, k := range a.Keep {
			clean := path.Clean(k)
			if k == "" || clean != k || k == "." || path.IsAbs(k) || clean == ".." || strings.HasPrefix(clean, "../") || !mountRE.MatchString(k) {
				bad = append(bad, fmt.Sprintf("%s.keep: %q must be a clean path relative to the folder (letters, digits, . _ - /), like node_modules", field, k))
			}
			// A macOS folder is case-insensitive: node_modules and Node_modules are one path.
			if seen[strings.ToLower(k)] {
				bad = append(bad, fmt.Sprintf("%s.keep: %q is listed twice", field, k))
			}
			seen[strings.ToLower(k)] = true
		}
	}
	return bad
}

// validateHost checks an absolute host path; a Fleet-file path is checked once a12s up resolves it.
func validateHost(host string) string {
	switch {
	case host == "":
		return "required: the folder on this machine to attach"
	case HostIsFileRelative(host):
		return ""
	case path.Clean(host) != host:
		return fmt.Sprintf("%q must be a clean absolute path", host)
	case host == "/":
		return "the whole filesystem can't be attached; name the project folder"
	}
	// Lowercased: macOS file systems are case-insensitive, so /VAR/RUN is /var/run.
	lower := strings.ToLower(host)
	if strings.HasSuffix(lower, ".sock") || slices.Contains(strings.Split(lower, "/"), ".docker") {
		return fmt.Sprintf("%q is or holds a Docker socket; attach a project folder", host)
	}
	for _, d := range socketDirs {
		if within(lower, d) || within(d, lower) {
			return fmt.Sprintf("%q is or contains %s, where the Docker socket lives; attach a project folder", host, d)
		}
	}
	return ""
}
