package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/fleet"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func labFleet(t *testing.T) *fleet.Fleet {
	t.Helper()
	f, err := fleet.Load("../fleet/testdata/lab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestBindingsResolve(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("A12S_TEST_GH", "ghp_secret")
	write(t, filepath.Join(home, ".a12s/lab/anthropic.json"), `{"claudeAiOauth":{}}`+"\n")
	dir := t.TempDir()
	write(t, filepath.Join(dir, "keys/openai.txt"), "sk-test\n")
	path := filepath.Join(dir, BindingsFile)
	write(t, path, `credentials:
  anthropic: { file: ~/.a12s/lab/anthropic.json }
  openai:    { file: keys/openai.txt }   # relative to the bindings file
  github:    { env: A12S_TEST_GH }
  unused:    { env: NOT_SET_ANYWHERE }   # never needed, never read
`)
	b, err := LoadBindings(path)
	if err != nil {
		t.Fatal(err)
	}
	f := labFleet(t)
	creds, err := b.Resolve(Needed(f))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"anthropic": `{"claudeAiOauth":{}}`, "openai": "sk-test", "github": "ghp_secret"}
	if len(creds) != len(want) {
		t.Fatalf("creds %v", creds)
	}
	for k, v := range want {
		if string(creds[k]) != v {
			t.Errorf("%s = %q, want %q", k, creds[k], v)
		}
	}

	// Only what one Computer needs.
	creds, err = b.Resolve(Needed(f, "scout"))
	if err != nil || len(creds) != 1 || creds["anthropic"] == nil {
		t.Errorf("scout creds %v, err %v", creds, err)
	}
}

func TestBindingsErrors(t *testing.T) {
	dir := t.TempDir()
	f := labFleet(t)

	// No file, credentials needed.
	b, err := LoadBindings(filepath.Join(dir, "missing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(Needed(f)); err == nil || !strings.Contains(err.Error(), "anthropic, github, openai") {
		t.Errorf("missing file: %v", err)
	}
	// No file, nothing needed.
	if c, err := b.Resolve(nil); err != nil || len(c) != 0 {
		t.Errorf("nothing needed: %v %v", c, err)
	}

	// Every unresolved name is reported, with who needs it.
	path := filepath.Join(dir, "b.yaml")
	t.Setenv("A12S_TEST_EMPTY", "")
	write(t, path, `credentials:
  anthropic: { file: nope.json }
  github: { env: A12S_TEST_EMPTY }
`)
	b, err = LoadBindings(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Resolve(Needed(f))
	if err == nil {
		t.Fatal("want error")
	}
	for _, s := range []string{
		"credential anthropic (needed by scout, tester)",
		"credential github (needed by builder): environment variable A12S_TEST_EMPTY is not set",
		"credential openai (needed by builder, reviewer) has no binding",
	} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q lacks %q", err, s)
		}
	}

	for name, content := range map[string]string{
		"both":    "credentials:\n  a: { file: x, env: Y }\n",
		"neither": "credentials:\n  a: {}\n",
		"unknown": "credentials:\n  a: { secret: x }\n",
		"typo":    "credential:\n  a: { env: X }\n",
	} {
		write(t, path, content)
		if _, err := LoadBindings(path); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	// An empty file is an empty set.
	write(t, path, "")
	if b, err := LoadBindings(path); err != nil || !b.Exists || len(b.Credentials) != 0 {
		t.Errorf("empty: %+v %v", b, err)
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/h")
	for in, want := range map[string]string{"~": "/h", "~/x/y": "/h/x/y", "/abs": "/abs", "rel/~": "rel/~", "~other": "~other"} {
		if got, _ := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}
