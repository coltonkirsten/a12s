package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubBin builds a PATH of symlinks to the real tools the script needs, plus
// stubs: chown is a no-op (the test is not root), touch fails when failTouch is
// set, and flock (when withFlock) just runs its command.
func stubBin(t *testing.T, failTouch, withFlock bool) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"sh", "mkdir", "chmod", "cat", "mv", "rm", "touch"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
		if tool == "touch" && failTouch {
			real = "/usr/bin/false"
			if _, err := os.Stat(real); err != nil {
				real = "/bin/false"
			}
		}
		if err := os.Symlink(real, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("chown", "exit 0")
	if withFlock {
		stub("flock", `shift 3; exec "$@"`)
	}
	return dir
}

func runWriteScript(t *testing.T, failTouch, withFlock bool) (string, error) {
	t.Helper()
	bin := stubBin(t, failTouch, withFlock)
	target := filepath.Join(t.TempDir(), "secrets", "anthropic")
	cmd := exec.Command(filepath.Join(bin, "sh"), "-c", writeScript, "sh", target, "0:0", "0600", "0755")
	cmd.Env = []string{"PATH=" + bin}
	cmd.Stdin = strings.NewReader("token")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("script output: %s", out)
	}
	return target, err
}

func TestWriteScriptPublishes(t *testing.T) {
	for _, withFlock := range []bool{false, true} {
		target, err := runWriteScript(t, false, withFlock)
		if err != nil {
			t.Fatalf("flock=%v: %v", withFlock, err)
		}
		if b, _ := os.ReadFile(target); string(b) != "token" {
			t.Fatalf("flock=%v: published %q", withFlock, b)
		}
	}
}

// A touch that fails after the rename must not leave the new secret in place
// with a pre-publication mtime: the write fails and the file is gone.
func TestWriteScriptTouchFailureRemovesPublishedFile(t *testing.T) {
	for _, withFlock := range []bool{false, true} {
		target, err := runWriteScript(t, true, withFlock)
		if err == nil {
			t.Fatalf("flock=%v: script succeeded despite failed touch", withFlock)
		}
		if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
			t.Fatalf("flock=%v: published file left behind (stat err %v)", withFlock, statErr)
		}
		leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".a12s-write.*"))
		if len(leftovers) != 0 {
			t.Fatalf("flock=%v: temp files left: %v", withFlock, leftovers)
		}
	}
}
