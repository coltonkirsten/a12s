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

const BindingsFile = "a12s.bindings.yaml"

// Bindings is per deployment and never committed.
type Bindings struct {
	Path        string             `yaml:"-"`
	Exists      bool               `yaml:"-"`
	Credentials map[string]Binding `yaml:"credentials"`
}

// Binding sets exactly one of File or Env.
type Binding struct {
	File string `yaml:"file,omitempty"`
	Env  string `yaml:"env,omitempty"`
}

// LoadBindings treats a missing file as empty; Resolve reports it if any
// credential is needed.
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

// Needed maps credential names to the Computers that reference them. No names
// means every Computer in the Fleet.
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

// ResolveBound skips unbound names: a Service waiting on a login should not
// block the rest. A binding that does not read is still an error.
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

func (b *Bindings) Resolve(need map[string][]string) (api.Credentials, error) {
	creds := api.Credentials{}
	if len(need) == 0 {
		return creds, nil
	}
	if !b.Exists {
		return nil, fmt.Errorf("the Fleet needs credentials %s but %s does not exist  fix: a12s creds set %s (or copy %s.example and fill it in)",
			strings.Join(sortedKeys(need), ", "), b.Path, sortedKeys(need)[0], BindingsFile)
	}
	var errs []error
	for _, name := range sortedKeys(need) {
		who := strings.Join(need[name], ", ")
		s, ok := b.Credentials[name]
		if !ok {
			errs = append(errs, fmt.Errorf("credential %s (needed by %s) has no binding  fix: a12s creds set %s", name, who, name))
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
