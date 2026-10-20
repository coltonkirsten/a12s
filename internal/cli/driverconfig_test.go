package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver/gce"
)

func writeDriverYAML(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".a12s", "lab")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, driverFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDriverConfigAPIListen(t *testing.T) {
	writeDriverYAML(t, "driver: gce\ngce:\n  subnetwork: a12s-lab\n  api: { listen: internal }\n")
	cfg, err := ReadDriverConfig("lab")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GCE.API == nil || cfg.GCE.API.Listen != gce.APIListenInternal {
		t.Fatalf("api = %+v", cfg.GCE.API)
	}
	want := `A12S_GCE={"subnetwork":"a12s-lab","api":{"listen":"internal"}}`
	if env := cfg.env(); !slices.Contains(env, want) {
		t.Errorf("env %v lacks %s", env, want)
	}

	for body, wantErr := range map[string]string{
		"driver: gce\ngce:\n  subnetwork: a12s-lab\n  api: { listen: public }\n": `gce.api.listen "public"`,
		"driver: gce\ngce:\n  subnetwork: a12s-lab\n  api: {}\n":                 `gce.api.listen ""`,
		"driver: gce\ngce:\n  subnetwork: a12s-lab\n  api: { port: 8443 }\n":     "port",
		"driver: gce\ngce:\n  api: { listen: internal }\n":                       "gce.subnetwork is required",
	} {
		writeDriverYAML(t, body)
		if _, err := ReadDriverConfig("lab"); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%q: err = %v, want %q", body, err, wantErr)
		}
	}
}

// Upgrading a12s must not replace a running Controller whose driver.yaml doesn't use the option.
func TestDriverDigestWithoutAPIUnchanged(t *testing.T) {
	writeDriverYAML(t, "driver: gce\ngce:\n  subnetwork: a12s-lab\n")
	cfg, err := ReadDriverConfig("lab")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.digest(), "abdb15df4e0c37cb"; got != want {
		t.Errorf("digest = %s, want %s", got, want)
	}
	if env := cfg.env(); !slices.Contains(env, `A12S_GCE={"subnetwork":"a12s-lab"}`) {
		t.Errorf("env %v", env)
	}
}
