package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSecretAndBind(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lab", "gh-oauth")
	if err := WriteSecret(p, []byte("gho_1")); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	b := filepath.Join(dir, "a12s.bindings.yaml")
	os.WriteFile(b, []byte("# keep me\ncredentials:\n  anthropic: { file: ~/.a12s/anthropic }\n  gh-oauth: { env: OLD }\n"), 0o644)
	if err := BindFile(b, "gh-oauth", p); err != nil {
		t.Fatal(err)
	}
	if err := BindFile(b, "postgres", filepath.Join(dir, "pg")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(b)
	s := string(data)
	if !strings.Contains(s, "# keep me") || !strings.Contains(s, "anthropic: {file: ~/.a12s/anthropic}") || strings.Contains(s, "OLD") || !strings.Contains(s, "gh-oauth: {file: "+p+"}") {
		t.Fatalf("bindings:\n%s", s)
	}
	bs, err := LoadBindings(b)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := bs.Resolve(map[string][]string{"gh-oauth": {"gh"}})
	if err != nil || string(creds["gh-oauth"]) != "gho_1" {
		t.Fatalf("resolve %v %v", creds, err)
	}
	// A fresh file is created 0600.
	nb := filepath.Join(dir, "new.yaml")
	if err := BindFile(nb, "x", p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(nb); fi.Mode().Perm() != 0o600 {
		t.Fatalf("new bindings mode %v", fi.Mode())
	}
}
