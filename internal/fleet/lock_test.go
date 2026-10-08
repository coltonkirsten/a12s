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

func TestLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)
	l, err := LoadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l, NewLock()) {
		t.Fatalf("missing lock should be empty, got %+v", l)
	}

	if !l.PinImage("slim", "sha256:s1") || !l.PinBundle("claude-code", "2.1.280", "sha256:c1") ||
		!l.PinBundle("codex", "0.154.0", "sha256:x1") || !l.PinPreset("./presets/scout", "sha256:p1") {
		t.Fatal("first pins should change the lock")
	}
	if l.PinImage("slim", "sha256:other") || l.PinBundle("codex", "0.154.0", "sha256:other") {
		t.Fatal("an existing pin must never change")
	}
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `images:
  slim: sha256:s1
bundles:
  claude-code@2.1.280: sha256:c1
  codex@0.154.0: sha256:x1
presets:
  ./presets/scout: sha256:p1
`
	if string(data) != want {
		t.Errorf("lock file:\n%s\nwant:\n%s", data, want)
	}
	back, err := LoadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, l) {
		t.Errorf("round trip: got %+v want %+v", back, l)
	}
}

func TestLoadLockPlanShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFile)
	doc := `images:  { slim: "sha256:a", browser: "sha256:b" }
bundles: { "claude-code@2.1.280": "sha256:c", "codex@0.154.0": "sha256:d" }
presets: { "./presets/scout": "sha256:e" }   # content hash of the directory
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &Lock{
		Images:   map[string]string{"slim": "sha256:a", "browser": "sha256:b"},
		Bundles:  map[string]string{"claude-code@2.1.280": "sha256:c", "codex@0.154.0": "sha256:d"},
		Presets:  map[string]string{"./presets/scout": "sha256:e"},
		Services: map[string]string{},
	}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("got %+v want %+v", l, want)
	}

	if err := os.WriteFile(path, []byte("images: {}\nsurprise: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLock(path); err == nil {
		t.Error("unknown lock field should fail")
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
