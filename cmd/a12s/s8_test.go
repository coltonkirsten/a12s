package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultFleetPathSearchesUpward(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "fleet.yaml"), []byte("x"), 0o644)
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(sub)
	t.Setenv("A12S_FLEET", "")
	if p := defaultFleetPath(); p != filepath.Join("..", "..", "fleet.yaml") {
		t.Fatalf("got %q", p)
	}
	t.Setenv("A12S_FLEET", "/elsewhere/fleet.yaml")
	if p := defaultFleetPath(); p != "/elsewhere/fleet.yaml" {
		t.Fatalf("env: %q", p)
	}
}

func TestFleetFlags(t *testing.T) {
	g := &globals{fleetPath: "fleet.yaml"}
	fs := g.flags("x")
	if _, err := parse(fs, []string{"-F", "a.yaml", "--json"}); err != nil || g.fleetPath != "a.yaml" || !g.json {
		t.Fatalf("%v %+v", err, g)
	}
	if _, err := parse(fs, []string{"--fleet", "b.yaml"}); err != nil || g.fleetPath != "b.yaml" {
		t.Fatalf("%v %+v", err, g)
	}
	if _, err := parse(fs, []string{"-f", "c.yaml"}); err != nil || g.fleetPath != "c.yaml" || !g.warnedF {
		t.Fatalf("deprecated -f: %v %+v", err, g)
	}
	var follow bool
	g2 := &globals{fleetPath: "fleet.yaml"}
	if _, err := parse(g2.flagsFollow("logs", &follow), []string{"-f", "scout"}); err != nil || !follow || g2.fleetPath != "fleet.yaml" {
		t.Fatalf("logs -f: %v %v %+v", err, follow, g2)
	}
}

func TestFetchesAtStart(t *testing.T) {
	if !fetchesAtStart(map[string]any{"command": []any{"npx", "-y", "@x/server"}}) || !fetchesAtStart(map[string]any{"command": "/usr/local/bin/npx"}) {
		t.Fatal("npx not caught")
	}
	if fetchesAtStart(map[string]any{"command": []any{"/opt/mcp/bin/mcp-server-postgres"}}) {
		t.Fatal("baked server flagged")
	}
}
