package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// seccompProfile is the subset of Docker's seccomp profile format a12s accepts. Unknown fields are refused rather
// than passed through, so a typo cannot silently drop a rule.
type seccompProfile struct {
	DefaultAction   string            `json:"defaultAction"`
	DefaultErrnoRet *uint             `json:"defaultErrnoRet,omitempty"`
	Architectures   []string          `json:"architectures,omitempty"`
	ArchMap         []json.RawMessage `json:"archMap,omitempty"`
	Syscalls        []seccompSyscall  `json:"syscalls,omitempty"`
	Flags           []string          `json:"flags,omitempty"`
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

// HashSeccomp is the digest fleet.lock records for a profile.
func HashSeccomp(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
