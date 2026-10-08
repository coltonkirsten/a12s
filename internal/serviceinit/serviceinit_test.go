//go:build unix

package serviceinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseUser(t *testing.T) {
	if u, g, err := parseUser("999:998"); err != nil || u != 999 || g != 998 {
		t.Fatalf("%d %d %v", u, g, err)
	}
	for _, bad := range []string{"", "0:0", "999", "a:b", "-1:5"} {
		if _, _, err := parseUser(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestExportConfigEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"env":{"A12S_TEST_X":"x y","A12S_TEST_N":5,"A12S_TEST_L":["a"]},"jobs":[]}`), 0o644)
	if err := exportConfigEnv(p); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("A12S_TEST_X") != "x y" || os.Getenv("A12S_TEST_N") != "5" || os.Getenv("A12S_TEST_L") != `["a"]` {
		t.Fatalf("env %q %q %q", os.Getenv("A12S_TEST_X"), os.Getenv("A12S_TEST_N"), os.Getenv("A12S_TEST_L"))
	}
	os.WriteFile(p, []byte(`{"env":{"A=B":"x"}}`), 0o644)
	if err := exportConfigEnv(p); err == nil {
		t.Fatal("a key with = accepted")
	}
	if err := exportConfigEnv(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("absent config: %v", err)
	}
}

func TestWriteFileOnlyUnderRun(t *testing.T) {
	for _, p := range []string{"/etc/passwd", "/run/a12s/../etc/x", "relative", "/run/a12sx/y"} {
		if err := WriteFile(p, "0:0", "0600", "0700", strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "only paths under") {
			t.Errorf("%s: %v", p, err)
		}
	}
}
