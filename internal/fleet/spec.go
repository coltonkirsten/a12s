package fleet

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Spec is a Fleet as written: defaults and each agent computer before the defaults are merged in. The Controller
// stores it, GET /v1/fleet returns it, and per-agent-computer writes change one entry of it.
type Spec struct {
	APIVersion     string                     `json:"apiVersion"`
	Kind           string                     `json:"kind"`
	Metadata       Metadata                   `json:"metadata"`
	Egress         FleetEgress                `json:"egress,omitzero"`
	Defaults       json.RawMessage            `json:"defaults,omitempty"`
	Drives         map[string]Drive           `json:"drives,omitempty"`
	AgentComputers map[string]json.RawMessage `json:"agent_computers"`
	Services       map[string]Service         `json:"services,omitempty"`
	// LegacyKey is set when the source used the old `computers` key.
	LegacyKey bool `json:"-"`
}

type specAlias Spec

// UnmarshalJSON is strict, and takes the old `computers` key for one release.
func (s *Spec) UnmarshalJSON(b []byte) error {
	var v struct {
		specAlias
		Computers map[string]json.RawMessage `json:"computers"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	*s = Spec(v.specAlias)
	if v.Computers != nil {
		if s.AgentComputers != nil {
			return errors.New("fleet: both agent_computers and computers are set; computers is the old name of agent_computers")
		}
		s.AgentComputers, s.LegacyKey = v.Computers, true
	}
	if s.AgentComputers == nil {
		s.AgentComputers = map[string]json.RawMessage{}
	}
	return nil
}

func (s *Spec) Names() []string { return sortedKeys(s.AgentComputers) }

// Clone is a deep copy.
func (s *Spec) Clone() *Spec {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var out Spec
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

// Effective merges the defaults into every agent computer (maps merge, lists and scalars replace) without
// validating.
func (s *Spec) Effective() (*Fleet, error) {
	f := &Fleet{
		APIVersion: s.APIVersion,
		Kind:       s.Kind,
		Metadata:   s.Metadata,
		Egress:     s.Egress,
		Drives:     s.Drives,
		Services:   s.Services,
		Computers:  make(map[string]Computer, len(s.AgentComputers)),
	}
	defaults, err := objectOf(s.Defaults)
	if err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	if err := decodeComputerJSON(defaults, &f.Defaults); err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	var errs []error
	for _, name := range s.Names() {
		own, err := objectOf(s.AgentComputers[name])
		if err != nil {
			errs = append(errs, fmt.Errorf("agent_computers.%s: %w", name, err))
			continue
		}
		var c Computer
		if err := decodeComputerJSON(MergeJSON(defaults, own), &c); err != nil {
			errs = append(errs, fmt.Errorf("agent_computers.%s: %w", name, err))
			continue
		}
		if c.Autonomy == "" {
			c.Autonomy = AutonomyFull
		}
		f.Computers[name] = c
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return f, nil
}

// Resolve is Effective then Validate.
func (s *Spec) Resolve() (*Fleet, error) {
	f, err := s.Effective()
	if err != nil {
		return nil, err
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f, nil
}

func decodeComputerJSON(obj map[string]any, c *Computer) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(c)
}

// SpecOf is one Computer's JSON as it goes into a Spec.
func SpecOf(c Computer) json.RawMessage {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return b
}

// objectOf decodes a JSON object keeping numbers exact; empty or null is an empty object.
func objectOf(raw json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(t))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("want a JSON object: %v", err)
	}
	return out, nil
}

// MergeJSON lays over onto base: objects merge key by key, anything else (lists, scalars, null) replaces. Neither
// input is modified.
func MergeJSON(base, over map[string]any) map[string]any {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range over {
		bo, bok := out[k].(map[string]any)
		vo, vok := v.(map[string]any)
		if bok && vok {
			out[k] = MergeJSON(bo, vo)
			continue
		}
		out[k] = v
	}
	return out
}

// MergePatch applies an RFC 7386 JSON merge patch: null deletes a key, objects merge, anything else replaces.
func MergePatch(target json.RawMessage, patch json.RawMessage) (json.RawMessage, error) {
	t, err := objectOf(target)
	if err != nil {
		return nil, err
	}
	p, err := objectOf(patch)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(mergePatch(t, p))
	if err != nil {
		return nil, err
	}
	return b, nil
}

func mergePatch(t, p map[string]any) map[string]any {
	out := maps.Clone(t)
	for k, v := range p {
		if v == nil {
			delete(out, k)
			continue
		}
		vo, vok := v.(map[string]any)
		to, tok := out[k].(map[string]any)
		switch {
		case vok && tok:
			out[k] = mergePatch(to, vo)
		case vok:
			out[k] = mergePatch(map[string]any{}, vo)
		default:
			out[k] = v
		}
	}
	return out
}

// Canonical is a Spec entry re-encoded so equal specs compare equal byte for byte.
func Canonical(raw json.RawMessage) string {
	obj, err := objectOf(raw)
	if err != nil {
		return string(raw)
	}
	b, _ := json.Marshal(obj)
	return string(b)
}

const (
	// MaxFilesBytes bounds instructions plus files of one agent computer.
	MaxFilesBytes      = 1 << 20
	InstructionsClaude = "CLAUDE.md"
	InstructionsCodex  = "AGENTS.md"
)

// InstructionsFile is where a harness reads its standing instructions.
func InstructionsFile(kind string) string {
	if kind == HarnessCodex {
		return InstructionsCodex
	}
	return InstructionsClaude
}

// HomeFiles are the files written onto the home drive: files plus the instructions file.
func (c Computer) HomeFiles() map[string]string {
	if c.Instructions == "" && len(c.Files) == 0 {
		return nil
	}
	out := maps.Clone(c.Files)
	if out == nil {
		out = map[string]string{}
	}
	if c.Instructions != "" {
		out[InstructionsFile(c.Harness.Kind)] = c.Instructions
	}
	return out
}

func validateFiles(c Computer) []string {
	var bad []string
	total := len(c.Instructions)
	for _, p := range slices.Sorted(maps.Keys(c.Files)) {
		total += len(p) + len(c.Files[p])
		clean := filepath.ToSlash(filepath.Clean(p))
		if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || clean != p || clean == ".." || strings.HasPrefix(clean, "../") {
			bad = append(bad, fmt.Sprintf("files: %q must be a clean path relative to the home directory", p))
		}
	}
	if c.Instructions != "" {
		name := InstructionsFile(c.Harness.Kind)
		if _, ok := c.Files[name]; ok {
			bad = append(bad, fmt.Sprintf("instructions: also set as files[%q] (a preset's %s?); keep one", name, name))
		}
	}
	if total > MaxFilesBytes {
		bad = append(bad, fmt.Sprintf("files: instructions and files are %d bytes, more than 1 MiB", total))
	}
	return bad
}

// InlineFiles reads a preset directory into text files, for a12s up. Directories become implicit, and a file that
// is not UTF-8 text is an error: the API carries text.
func InlineFiles(dir string) (map[string]string, error) {
	entries, err := walkPreset(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		switch e.typ {
		case tar.TypeSymlink:
			return nil, fmt.Errorf("preset %s: %s is a symlink; a12s sends presets as text files", dir, e.rel)
		case tar.TypeReg:
			b, err := os.ReadFile(e.abs)
			if err != nil {
				return nil, err
			}
			if !isText(b) {
				return nil, fmt.Errorf("preset %s: %s is not UTF-8 text; a12s sends presets as text files", dir, e.rel)
			}
			out[e.rel] = string(b)
		}
	}
	return out, nil
}

func isText(b []byte) bool { return utf8.Valid(b) && !bytes.ContainsRune(b, 0) }

// FilesTar is the tar the driver copies onto the home drive, and its digest. Directories are implied; a file that
// starts with "#!" is executable. The same files always give the same bytes.
func FilesTar(files map[string]string) ([]byte, string, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			d := strings.Join(parts[:i], "/")
			if dirs[d] {
				continue
			}
			dirs[d] = true
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, Uid: AgentUID, Gid: AgentGID, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
				return nil, "", err
			}
		}
		mode := int64(0o644)
		if strings.HasPrefix(files[p], "#!") {
			mode = 0o755
		}
		body := []byte(files[p])
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: mode, Size: int64(len(body)), Uid: AgentUID, Gid: AgentGID, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), "sha256:" + hex.EncodeToString(sum[:]), nil
}
