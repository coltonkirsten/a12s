package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/cli"
)

// ui runs the console through npx with the Fleet's name and the caller's arguments, and exits with its code.
func TestUIRunsConsoleWithNpx(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := cli.WriteControllerFile("lab", &api.ControllerFile{Port: 1, Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nexit 3\n"
	if err := os.WriteFile(filepath.Join(bin, "npx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	fleetFile := "../../internal/fleet/testdata/lab.yaml"

	var b bytes.Buffer
	if code := run([]string{"-F", fleetFile, "ui", "--port", "7013", "--no-open"}, &b); code != 3 {
		t.Fatalf("exit %d, want the console's 3: %s", code, b.String())
	}
	got, _ := os.ReadFile(argsFile)
	if want := "--yes\ngithub:coltonkirsten/a12s-console\n--fleet\nlab\n--port\n7013\n--no-open\n"; string(got) != want {
		t.Errorf("npx args %q, want %q", got, want)
	}

	t.Setenv("A12S_CONSOLE", "./console")
	if code := run([]string{"-F", fleetFile, "ui"}, &b); code != 3 {
		t.Fatalf("exit %d: %s", code, b.String())
	}
	if got, _ := os.ReadFile(argsFile); string(got) != "--yes\n./console\n--fleet\nlab\n" {
		t.Errorf("npx args with A12S_CONSOLE %q", got)
	}

	t.Setenv("PATH", t.TempDir())
	b.Reset()
	if code := run([]string{"-F", fleetFile, "ui"}, &b); code != 1 || !strings.Contains(b.String(), "install Node.js 20+") {
		t.Errorf("without npx: exit %d: %s", code, b.String())
	}
}
