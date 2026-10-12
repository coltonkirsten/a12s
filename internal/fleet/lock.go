package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const LockFile = "fleet.lock"

// Lock entries are only ever added; an existing pin changes only by editing the file.
type Lock struct {
	// A12s is the a12s release this Fleet runs on; a release binary of another version refuses to apply it.
	A12s     string            `yaml:"a12s,omitempty" json:"a12s,omitempty"`
	Images   map[string]string `yaml:"images" json:"images"`
	Bundles  map[string]string `yaml:"bundles" json:"bundles"`
	Presets  map[string]string `yaml:"presets" json:"presets"`
	Services map[string]string `yaml:"services,omitempty" json:"services,omitempty"`
	// Seccomp is each security.seccomp profile's digest, by its path in the Fleet file.
	Seccomp map[string]string `yaml:"seccomp,omitempty" json:"seccomp,omitempty"`
}

func ServiceKey(name, ref string) string { return name + "@" + ref }

func BundleKey(kind, version string) string { return kind + "@" + version }

func NewLock() *Lock {
	return &Lock{Images: map[string]string{}, Bundles: map[string]string{}, Presets: map[string]string{}, Services: map[string]string{}, Seccomp: map[string]string{}}
}

// LoadLock treats a missing file as an empty lock.
func LoadLock(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewLock(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read lock: %w", err)
	}
	l := NewLock()
	if err := decodeStrict(data, l); err != nil && !errors.Is(err, errEmptyDocument) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	l.fillMaps()
	return l, nil
}

// Save writes atomically so a crash never leaves a truncated lock.
func (l *Lock) Save(path string) error {
	l.fillMaps()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(l); err != nil {
		return fmt.Errorf("encode lock: %w", err)
	}
	data := buf.Bytes()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fleet.lock-*")
	if err != nil {
		return fmt.Errorf("write lock: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write lock: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write lock: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("write lock: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write lock: %w", err)
	}
	return nil
}

func (l *Lock) fillMaps() {
	if l.Images == nil {
		l.Images = map[string]string{}
	}
	if l.Bundles == nil {
		l.Bundles = map[string]string{}
	}
	if l.Presets == nil {
		l.Presets = map[string]string{}
	}
	if l.Services == nil {
		l.Services = map[string]string{}
	}
	if l.Seccomp == nil {
		l.Seccomp = map[string]string{}
	}
}

// PinImage never moves an existing pin. It reports whether the lock changed.
func (l *Lock) PinImage(key, digest string) bool { return pin(&l.Images, key, digest) }

func (l *Lock) PinBundle(kind, version, digest string) bool {
	return pin(&l.Bundles, BundleKey(kind, version), digest)
}

func (l *Lock) PinPreset(path, digest string) bool { return pin(&l.Presets, path, digest) }

// SetPreset records a preset's current digest, replacing an older one.
func (l *Lock) SetPreset(path, digest string) {
	if l.Presets == nil {
		l.Presets = map[string]string{}
	}
	l.Presets[path] = digest
}

// SetSeccomp records a profile's current digest, replacing an older one: the profile is the project's own source.
func (l *Lock) SetSeccomp(path, digest string) {
	if l.Seccomp == nil {
		l.Seccomp = map[string]string{}
	}
	l.Seccomp[path] = digest
}

func (l *Lock) PinService(name, ref, digest string) bool {
	return pin(&l.Services, ServiceKey(name, ref), digest)
}

func pin(m *map[string]string, key, digest string) bool {
	if *m == nil {
		*m = map[string]string{}
	}
	if _, ok := (*m)[key]; ok {
		return false
	}
	(*m)[key] = digest
	return true
}
