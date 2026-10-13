package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// seccompProfile is the subset of Docker's seccomp profile format a12s accepts. Unknown fields are refused rather
// than passed through, so a typo cannot silently drop a rule.
type seccompProfile struct {
	DefaultAction   string           `json:"defaultAction"`
	DefaultErrnoRet *uint            `json:"defaultErrnoRet,omitempty"`
	Architectures   []string         `json:"architectures,omitempty"`
	ArchMap         []seccompArch    `json:"archMap,omitempty"`
	Syscalls        []seccompSyscall `json:"syscalls,omitempty"`
	Flags           []string         `json:"flags,omitempty"`
}

type seccompArch struct {
	Architecture     string   `json:"architecture"`
	SubArchitectures []string `json:"subArchitectures,omitempty"`
}

// seccompArches are the names runc's libseccomp accepts; any other name makes the container fail to start, long after
// the write was accepted.
var seccompArches = map[string]bool{
	"SCMP_ARCH_X86": true, "SCMP_ARCH_X86_64": true, "SCMP_ARCH_X32": true, "SCMP_ARCH_ARM": true,
	"SCMP_ARCH_AARCH64": true, "SCMP_ARCH_MIPS": true, "SCMP_ARCH_MIPS64": true, "SCMP_ARCH_MIPS64N32": true,
	"SCMP_ARCH_MIPSEL": true, "SCMP_ARCH_MIPSEL64": true, "SCMP_ARCH_MIPSEL64N32": true, "SCMP_ARCH_PPC": true,
	"SCMP_ARCH_PPC64": true, "SCMP_ARCH_PPC64LE": true, "SCMP_ARCH_S390": true, "SCMP_ARCH_S390X": true,
	"SCMP_ARCH_PARISC": true, "SCMP_ARCH_PARISC64": true, "SCMP_ARCH_RISCV64": true, "SCMP_ARCH_LOONGARCH64": true,
	"SCMP_ARCH_M68K": true, "SCMP_ARCH_SH": true, "SCMP_ARCH_SHEB": true,
}

type seccompSyscall struct {
	Name     string            `json:"name,omitempty"`
	Names    []string          `json:"names,omitempty"`
	Action   string            `json:"action"`
	Args     []json.RawMessage `json:"args,omitempty"`
	ErrnoRet *uint             `json:"errnoRet,omitempty"`
	Comment  string            `json:"comment,omitempty"`
	Includes json.RawMessage   `json:"includes,omitempty"`
	Excludes json.RawMessage   `json:"excludes,omitempty"`
}

// SCMP_ACT_NOTIFY is left out: it hands syscall decisions to a listener process outside the Computer.
var seccompActions = map[string]bool{
	"SCMP_ACT_KILL": true, "SCMP_ACT_KILL_PROCESS": true, "SCMP_ACT_KILL_THREAD": true, "SCMP_ACT_TRAP": true,
	"SCMP_ACT_ERRNO": true, "SCMP_ACT_TRACE": true, "SCMP_ACT_ALLOW": true, "SCMP_ACT_LOG": true,
}

// ValidateSeccomp checks that data is a seccomp profile the substrate can load. It reports whether the profile
// allows every syscall it does not list, which is worth a warning.
func ValidateSeccomp(data []byte) (allowByDefault bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p seccompProfile
	if err := dec.Decode(&p); err != nil {
		return false, fmt.Errorf("not a seccomp profile (Docker's JSON format): %v", err)
	}
	if dec.More() {
		return false, errors.New("not a seccomp profile: more than one JSON value")
	}
	if !seccompActions[p.DefaultAction] {
		return false, fmt.Errorf("defaultAction %q is not a seccomp action (SCMP_ACT_ERRNO, SCMP_ACT_ALLOW, ...)", p.DefaultAction)
	}
	arch := func(field, a string) error {
		if !seccompArches[a] {
			return fmt.Errorf("%s %q is not a seccomp architecture (SCMP_ARCH_X86_64, SCMP_ARCH_AARCH64, SCMP_ARCH_ARM, ...)", field, a)
		}
		return nil
	}
	for i, a := range p.Architectures {
		if err := arch(fmt.Sprintf("architectures[%d]", i), a); err != nil {
			return false, err
		}
	}
	for i, m := range p.ArchMap {
		if err := arch(fmt.Sprintf("archMap[%d].architecture", i), m.Architecture); err != nil {
			return false, err
		}
		for j, a := range m.SubArchitectures {
			if err := arch(fmt.Sprintf("archMap[%d].subArchitectures[%d]", i, j), a); err != nil {
				return false, err
			}
		}
	}
	if len(p.Syscalls) == 0 && p.DefaultAction != "SCMP_ACT_ALLOW" {
		return false, errors.New("the profile lists no syscalls, so nothing could run")
	}
	for i, s := range p.Syscalls {
		if s.Name == "" && len(s.Names) == 0 {
			return false, fmt.Errorf("syscalls[%d] names no syscall", i)
		}
		if !seccompActions[s.Action] {
			return false, fmt.Errorf("syscalls[%d].action %q is not a supported seccomp action", i, s.Action)
		}
	}
	return p.DefaultAction == "SCMP_ACT_ALLOW", nil
}

// HashSeccomp is a profile's digest, as status reports it.
func HashSeccomp(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Seccomp is a security.seccomp value. In a Fleet file it is a path relative to the file (or the profile itself as a
// mapping); everywhere else it is the profile JSON. a12s up reads the file, so the Controller only ever sees JSON.
type Seccomp struct {
	Path    string
	Profile json.RawMessage
}

func (s Seccomp) IsZero() bool { return s.Path == "" && len(s.Profile) == 0 }

// Digest identifies the profile; "" when there is none.
func (s Seccomp) Digest() string {
	if len(s.Profile) == 0 {
		return ""
	}
	return HashSeccomp(s.Profile)
}

func (s Seccomp) MarshalJSON() ([]byte, error) {
	if len(s.Profile) > 0 {
		return s.Profile, nil
	}
	return json.Marshal(s.Path)
}

func (s *Seccomp) UnmarshalJSON(b []byte) error {
	*s = Seccomp{}
	switch {
	case bytes.Equal(bytes.TrimSpace(b), []byte("null")):
		return nil
	case len(b) > 0 && b[0] == '"':
		return json.Unmarshal(b, &s.Path)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return err
	}
	s.Profile = buf.Bytes()
	return nil
}

func (s *Seccomp) UnmarshalYAML(n *yaml.Node) error {
	*s = Seccomp{}
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "!!null" {
			return nil
		}
		s.Path = n.Value
		return nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("security.seccomp: %v", err)
	}
	s.Profile = b
	return nil
}

func (s Seccomp) MarshalYAML() (any, error) {
	if len(s.Profile) == 0 {
		return s.Path, nil
	}
	var v any
	if err := json.Unmarshal(s.Profile, &v); err != nil {
		return nil, err
	}
	return v, nil
}
