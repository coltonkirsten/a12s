package fleet

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// LockFile is the lock's file name, next to the Fleet file.
const LockFile = "fleet.lock"

// Lock pins image, bundle and preset digests (PLAN "HarnessBundle image").
// Entries are only ever added; an existing pin changes only by editing the file.
type Lock struct {
	// Images maps a tier ("slim", "browser") or a custom image reference to an image ID.
	Images map[string]string `yaml:"images" json:"images"`
	// Bundles maps "kind@version" to the HarnessBundle image ID.
	Bundles map[string]string `yaml:"bundles" json:"bundles"`
	// Presets maps a preset path, as written in the Fleet, to its content hash.
	Presets map[string]string `yaml:"presets" json:"presets"`
}

// BundleKey returns the Lock.Bundles key for a harness kind and version.
func BundleKey(kind, version string) string { return kind + "@" + version }

// NewLock returns an empty lock.
func NewLock() *Lock {
	return &Lock{Images: map[string]string{}, Bundles: map[string]string{}, Presets: map[string]string{}}
}

// LoadLock reads a lock file. A missing file is an empty lock.
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

// Save writes the lock to path atomically.
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
}

// PinImage records digest for an image key unless it is already pinned.
// It reports whether the lock changed.
func (l *Lock) PinImage(key, digest string) bool { return pin(&l.Images, key, digest) }

// PinBundle records digest for kind@version unless it is already pinned.
func (l *Lock) PinBundle(kind, version, digest string) bool {
	return pin(&l.Bundles, BundleKey(kind, version), digest)
}

// PinPreset records the content hash of a preset path unless it is already pinned.
func (l *Lock) PinPreset(path, digest string) bool { return pin(&l.Presets, path, digest) }

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

// Missing lists the lock keys the Fleet needs that are not yet pinned, in
// Computer name order without duplicates.
func (l *Lock) Missing(f *Fleet) (images, bundles, presets []string) {
	for _, name := range f.ComputerNames() {
		c := f.Computers[name]
		images = appendMissing(images, l.Images, c.Image)
		bundles = appendMissing(bundles, l.Bundles, c.Harness.BundleKey())
		if c.Home.Preset != "" {
			presets = appendMissing(presets, l.Presets, c.Home.Preset)
		}
	}
	return images, bundles, presets
}

func appendMissing(list []string, pinned map[string]string, key string) []string {
	if _, ok := pinned[key]; ok || slices.Contains(list, key) {
		return list
	}
	return append(list, key)
}
