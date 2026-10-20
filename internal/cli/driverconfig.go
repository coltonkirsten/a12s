package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/coltonkirsten/a12s/internal/driver/docker"
	"github.com/coltonkirsten/a12s/internal/driver/gce"
)

const driverFileName = "driver.yaml"

// LabelDriver on the Controller container is the digest of the driver config it runs with: a change replaces it.
const LabelDriver = "a12s.driver"

// DriverConfig is ~/.a12s/<fleet>/driver.yaml, the substrate a Fleet's Controller runs on. With no file, it is
// Docker. It is not part of the Fleet: the Fleet changes while the Controller runs, the substrate can't.
type DriverConfig struct {
	Driver string       `yaml:"driver" json:"driver"`
	GCE    gce.Settings `yaml:"gce,omitempty" json:"gce,omitempty"`
}

func ReadDriverConfig(fleetName string) (DriverConfig, error) {
	cfg := DriverConfig{Driver: docker.Name}
	dir, err := StateDir(fleetName)
	if err != nil {
		return cfg, err
	}
	p := filepath.Join(dir, driverFileName)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", p, err)
	}
	switch cfg.Driver {
	case docker.Name:
	case gce.Name:
		if err := cfg.GCE.Validate(); err != nil {
			return cfg, fmt.Errorf("%s: %w", p, err)
		}
	default:
		return cfg, fmt.Errorf("%s: driver %q: want %s or %s", p, cfg.Driver, docker.Name, gce.Name)
	}
	return cfg, nil
}

func (c DriverConfig) env() []string {
	if c.Driver != gce.Name {
		return nil
	}
	b, _ := json.Marshal(c.GCE)
	return []string{"A12S_DRIVER=" + gce.Name, "A12S_GCE=" + string(b)}
}

func (c DriverConfig) digest() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
