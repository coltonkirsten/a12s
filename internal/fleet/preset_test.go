package fleet

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHashPreset(t *testing.T) {
	files := map[string]string{
		".claude/settings.json": `{"theme":"dark"}`,
		"notes/readme.md":       "hello",
		"bin/tool":              "#!/bin/sh\n",
	}
	a, b := t.TempDir(), t.TempDir()
	writeTree(t, a, files)
	writeTree(t, b, files)
	// Different umask-style permissions on a non-executable file must not matter.
	if err := os.Chmod(filepath.Join(b, "notes/readme.md"), 0o664); err != nil {
		t.Fatal(err)
	}
	ha, err := HashPreset(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := HashPreset(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb || !strings.HasPrefix(ha, "sha256:") || len(ha) != len("sha256:")+64 {
		t.Fatalf("hashes differ or malformed: %s %s", ha, hb)
	}

	steps := []struct {
		name   string
		mutate func(dir string) error
	}{
		{"content", func(d string) error { return os.WriteFile(filepath.Join(d, "notes/readme.md"), []byte("hellO"), 0o644) }},
		{"exec bit", func(d string) error { return os.Chmod(filepath.Join(d, "bin/tool"), 0o755) }},
		{"new empty dir", func(d string) error { return os.Mkdir(filepath.Join(d, "empty"), 0o755) }},
		{"symlink", func(d string) error { return os.Symlink("notes/readme.md", filepath.Join(d, "link")) }},
		{"rename", func(d string) error { return os.Rename(filepath.Join(d, "notes"), filepath.Join(d, "docs")) }},
	}
	prev := hb
	for _, s := range steps {
		if err := s.mutate(b); err != nil {
			t.Fatal(err)
		}
		h, err := HashPreset(b)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if h == prev {
			t.Errorf("%s: hash did not change", s.name)
		}
		prev = h
	}

	if err := os.Symlink("/etc/passwd", filepath.Join(b, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := HashPreset(b); err == nil {
		t.Error("absolute symlink should be rejected")
	}
}

func TestWritePresetTar(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".claude/settings.json": "{}", "hello.txt": "hi"})
	var buf bytes.Buffer
	if err := WritePresetTar(dir, &buf); err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err := WritePresetTar(dir, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), again.Bytes()) {
		t.Error("tar output is not deterministic")
	}
	tr := tar.NewReader(&buf)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uid != AgentUID || h.Gid != AgentGID {
			t.Errorf("%s owned by %d:%d", h.Name, h.Uid, h.Gid)
		}
		names = append(names, h.Name)
	}
	want := []string{".claude/", ".claude/settings.json", "hello.txt"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("entries = %v, want %v", names, want)
	}
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
