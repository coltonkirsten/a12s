package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/cli"
)

func TestDownUsage(t *testing.T) {
	var b bytes.Buffer
	if code := run([]string{"down", "extra"}, &b); code != 2 || !strings.Contains(b.String(), "down takes no arguments") {
		t.Errorf("code %d: %s", code, b.String())
	}
	b.Reset()
	if code := run([]string{"down", "--yes"}, &b); code != 2 || !strings.Contains(b.String(), "--yes only applies to --purge") {
		t.Errorf("code %d: %s", code, b.String())
	}
}

// The Fleet name becomes ~/.a12s/<name>: one that is not a DNS label is
// refused before anything is touched.
func TestDownRefusesBadFleetName(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fleet.yaml")
	data := "apiVersion: a12s.ai/v1alpha1\nkind: Fleet\nmetadata:\n  name: ../evil\n"
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if code := run([]string{"-f", p, "down"}, &b); code != 1 || !strings.Contains(b.String(), "must be a lowercase DNS label") {
		t.Errorf("code %d: %s", code, b.String())
	}
}

func TestConfirmPurge(t *testing.T) {
	drives := []cli.Drive{{Name: "home-hello", Volume: "a12s-qs-home-hello"}, {Name: "notes", Volume: "a12s-qs-drive-notes"}}
	for in, want := range map[string]bool{"y\n": true, "YES\n": true, "\n": false, "n\n": false, "": false, "yep\n": false} {
		var out bytes.Buffer
		if got := confirmPurge(strings.NewReader(in), &out, "qs", drives, nil, ""); got != want {
			t.Errorf("answer %q: got %v", in, got)
		}
		// Every drive and its volume is named before the question.
		s := out.String()
		for _, d := range drives {
			if !strings.Contains(s, d.Name) || !strings.Contains(s, d.Volume) {
				t.Errorf("prompt does not list %s: %q", d.Name, s)
			}
		}
	}
}

func TestWriteDown(t *testing.T) {
	plan := &cli.DownPlan{Fleet: "qs", Containers: nil}
	res := &cli.DownResult{
		Containers: []string{"a12s-qs-controller", "a12s-qs-hello"},
		Networks:   []string{"a12s-qs"},
		Volumes:    []string{"a12s-qs-controller"},
		StateDir:   "/home/u/.a12s/qs",
		Kept:       []cli.Drive{{Name: "home-hello", Volume: "a12s-qs-home-hello"}},
	}
	plan.Networks = res.Networks
	var b bytes.Buffer
	writeDown(&b, "qs", plan, res)
	// Each removed thing and each kept drive is reported; down --json is
	// for scripts, so the wording is free.
	out := b.String()
	for _, want := range []string{"a12s-qs-controller", "a12s-qs-hello", "a12s-qs", "/home/u/.a12s/qs", "home-hello", "a12s-qs-home-hello", "kept"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	b.Reset()
	writeDown(&b, "qs", &cli.DownPlan{Fleet: "qs"}, &cli.DownResult{})
	if !strings.Contains(b.String(), "already down") {
		t.Errorf("got %q", b.String())
	}
	b.Reset()
	d := cli.Drive{Name: "notes", Volume: "a12s-qs-drive-notes"}
	writeDown(&b, "qs", &cli.DownPlan{Fleet: "qs", Drives: []cli.Drive{d}}, &cli.DownResult{Destroyed: []cli.Drive{d}})
	if s := b.String(); !strings.Contains(s, "destroyed") || !strings.Contains(s, "a12s-qs-drive-notes") || strings.Contains(s, "kept") {
		t.Errorf("got %q", b.String())
	}
}
