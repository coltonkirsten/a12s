// Package cli holds the pieces of the a12s command line that are worth
// testing on their own: bindings, the Controller client, the SSE stream,
// output formatting and starting the Controller container.
package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// BindingsFile is the default bindings file name, next to the Fleet file.
const BindingsFile = "a12s.bindings.yaml"

// Bindings maps each credential name to where it comes from on this machine
// (§3 "Where secrets come from"). The file is per deployment and never
// committed.
type Bindings struct {
	Path        string             `yaml:"-"`
	Exists      bool               `yaml:"-"`
	Credentials map[string]Binding `yaml:"credentials"`
}

// Binding is one credential source: exactly one of File or Env.
type Binding struct {
	File string `yaml:"file,omitempty"`
	Env  string `yaml:"env,omitempty"`
}

// LoadBindings reads a bindings file. A missing file is not an error here:
// it is an empty set, and Resolve reports it if any credential is needed.
func LoadBindings(path string) (*Bindings, error) {
	b := &Bindings{Path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read bindings: %w", err)
	}
	b.Exists = true
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(b); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var errs []error
	for _, name := range sortedKeys(b.Credentials) {
		s := b.Credentials[name]
		if (s.File == "") == (s.Env == "") {
			errs = append(errs, fmt.Errorf("credential %s: set exactly one of file or env", name))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", path, errors.Join(errs...))
	}
	return b, nil
}

// Needed maps each credential name the given Computers reference to the
// Computers that reference it. No names means every Computer in the Fleet.
func Needed(f *fleet.Fleet, computers ...string) map[string][]string {
	if len(computers) == 0 {
		computers = f.ComputerNames()
	}
	need := map[string][]string{}
	for _, c := range computers {
		spec, ok := f.Computers[c]
		if !ok {
			continue
		}
		for _, cred := range spec.Credentials {
			need[cred] = append(need[cred], c)
		}
	}
	return need
}

// NeededByServices maps each credential name the Fleet's Services (or the
// named ones) reference to those Services.
func NeededByServices(f *fleet.Fleet, services ...string) map[string][]string {
	if len(services) == 0 {
		services = f.ServiceNames()
	}
	need := map[string][]string{}
	for _, s := range services {
		spec, ok := f.Services[s]
		if !ok {
			continue
		}
		for _, cred := range spec.Credentials {
			need[cred] = append(need[cred], s)
		}
	}
	return need
}

// ResolveBound reads the material of every needed credential that has a
// binding and skips the rest: a Service credential without one is the
// Controller's call (a login the Service's manifest produces leaves only
// that Service waiting, enhancements §5.5). A binding that does not read
// is still an error.
func (b *Bindings) ResolveBound(need map[string][]string) (api.Credentials, error) {
	creds := api.Credentials{}
	var errs []error
	for _, name := range sortedKeys(need) {
		s, ok := b.Credentials[name]
		if !ok || !b.Exists {
			continue
		}
		v, err := b.read(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("credential %s (needed by %s): %w", name, strings.Join(need[name], ", "), err))
			continue
		}
		creds[name] = v
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", b.Path, errors.Join(errs...))
	}
	return creds, nil
}

// Resolve reads the material of every needed credential. Every name must
// resolve; the error lists all that do not, with the Computers needing them.
func (b *Bindings) Resolve(need map[string][]string) (api.Credentials, error) {
	creds := api.Credentials{}
	if len(need) == 0 {
		return creds, nil
	}
	if !b.Exists {
		return nil, fmt.Errorf("the Fleet needs credentials %s but %s does not exist (copy %s.example and fill it in)",
			strings.Join(sortedKeys(need), ", "), b.Path, BindingsFile)
	}
	var errs []error
	for _, name := range sortedKeys(need) {
		who := strings.Join(need[name], ", ")
		s, ok := b.Credentials[name]
		if !ok {
			errs = append(errs, fmt.Errorf("credential %s (needed by %s) has no binding", name, who))
			continue
		}
		v, err := b.read(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("credential %s (needed by %s): %w", name, who, err))
			continue
		}
		creds[name] = v
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", b.Path, errors.Join(errs...))
	}
	return creds, nil
}

func (b *Bindings) read(s Binding) ([]byte, error) {
	if s.Env != "" {
		v, ok := os.LookupEnv(s.Env)
		if !ok || v == "" {
			return nil, fmt.Errorf("environment variable %s is not set", s.Env)
		}
		return []byte(v), nil
	}
	p, err := ExpandHome(s.File)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(b.Path), p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", p)
	}
	return data, nil
}

// ExpandHome replaces a leading "~" or "~/" with the user's home directory.
func ExpandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
