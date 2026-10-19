package gce

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/coltonkirsten/a12s/internal/driver"
)

// specStore keeps each Computer's staged spec on the Controller's state disk. Instance metadata can't: a spec's
// files are up to 1 MiB and a metadata value at most 256 KB, and anyone who can read the instance can read its
// metadata. The instance's labels carry the generation and the spec's digest, so a stale or missing copy here is
// detected, never used.
type specStore struct{ dir string }

func specDigest(spec driver.ComputerSpec) string {
	b, _ := json.Marshal(spec)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func (s specStore) path(computer string) string { return filepath.Join(s.dir, computer+".json") }

func (s specStore) put(spec driver.ComputerSpec) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "."+spec.Name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(spec.Name))
}

// get returns the staged spec only if it is the one the instance's labels name.
func (s specStore) get(computer string, generation int64, digest string) (driver.ComputerSpec, error) {
	var spec driver.ComputerSpec
	b, err := os.ReadFile(s.path(computer))
	if errors.Is(err, fs.ErrNotExist) {
		return spec, fmt.Errorf("no staged spec for %s on the Controller: %w", computer, driver.ErrNotStaged)
	}
	if err != nil {
		return spec, err
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return spec, fmt.Errorf("staged spec for %s: %w", computer, err)
	}
	if spec.Generation != generation || specDigest(spec) != digest {
		return spec, fmt.Errorf("the Controller's staged spec for %s is not generation %d: %w", computer, generation, driver.ErrNotStaged)
	}
	return spec, nil
}

func (s specStore) remove(computer string) error {
	if err := os.Remove(s.path(computer)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
