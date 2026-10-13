package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/fleet"
)

// export then up's reading of the files gives back the same Fleet: nothing is lost on the way through the file.
func TestExportRoundTrips(t *testing.T) {
	long := strings.Repeat("Be thorough.\n", 200) + "  indented\ttab, trailing space \n\nno final newline"
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, app, docs := filepath.Join(root, "fleet"), filepath.Join(root, "app"), filepath.Join(root, "fleet", "docs")
	for _, d := range []string{dir, app, docs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	live := &fleet.Spec{
		APIVersion: fleet.APIVersion, Kind: fleet.Kind, Metadata: fleet.Metadata{Name: "t"},
		Defaults: json.RawMessage(`{"harness":{"kind":"codex","version":"0.154.0"},"image":"slim","credentials":["openai"],"egress":{"allow":[]}}`),
		AgentComputers: map[string]json.RawMessage{
			"short": json.RawMessage(`{"instructions":"Be brief."}`),
			"long":  json.RawMessage(`{"instructions":` + strings.TrimSpace(mustJSON(long)) + `,"files":{"skills/x/SKILL.md":"skill","bin/run":"#!/bin/sh\necho hi\n",".mcp.json":"{\"a\": 1}\n"}}`),
			"sec":   json.RawMessage(`{"security":{"seccomp":{"defaultAction":"SCMP_ACT_ALLOW"}}}`),
			"att":   json.RawMessage(`{"attach":{"project":{"host":` + mustJSON(app) + `,"keep":["node_modules"]},"docs":{"host":` + mustJSON(docs) + `,"readOnly":true}}}`),
		},
	}
	files, err := exportFiles(live, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files["profiles/sec.json"] == nil {
		t.Fatalf("instructions and files stay in fleet.yaml, a seccomp profile is its own file: %v", keys(files))
	}
	if y := string(files["fleet.yaml"]); !strings.Contains(y, "host: ../app") || !strings.Contains(y, "host: ./docs") {
		t.Errorf("attach hosts are written relative to the Fleet file:\n%s", y)
	}
	for rel, b := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := &globals{fleetPath: filepath.Join(dir, "fleet.yaml")}
	got, _, err := g.loadSpec()
	if err != nil {
		t.Fatalf("%v\n%s", err, files["fleet.yaml"])
	}
	want, _ := live.Resolve()
	have, _ := got.Resolve()
	for _, name := range live.Names() {
		w, h := fleet.SpecOf(want.Computers[name]), fleet.SpecOf(have.Computers[name])
		if fleet.Canonical(w) != fleet.Canonical(h) {
			t.Errorf("%s:\n live %s\n file %s", name, w, h)
		}
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// a12s up sends every attach host as the folder's real absolute path, however the Fleet file wrote it.
func TestResolveHost(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, app := filepath.Join(root, "fleet"), filepath.Join(root, "app")
	for _, d := range []string{dir, app} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(app, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, "code"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	g := &globals{fleetPath: filepath.Join(dir, "fleet.yaml")}
	for in, want := range map[string]string{
		"../app":     app,
		"../link":    app,
		app:          app,
		"~/code":     filepath.Join(home, "code"),
		"./../app/.": app,
	} {
		if got, err := g.resolveHost(in); err != nil || got != want {
			t.Errorf("resolveHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"../missing": "does not exist",
		"../file":    "not a folder",
	} {
		if _, err := g.resolveHost(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("resolveHost(%q) = %v, want %q", in, err, want)
		}
	}
}

// ~/.a12s holds every Fleet's owner token and ~/.docker Docker Desktop's socket: neither may be attached, nor any
// folder holding them, such as the home directory itself.
func TestResolveHostRefusesHomeSecrets(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".a12s/team", ".docker/run", "code/app"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	g := &globals{fleetPath: filepath.Join(home, "code", "fleet.yaml")}
	for _, in := range []string{"~", "~/.a12s", "~/.a12s/team", "~/.docker/run", ".."} {
		if _, err := g.resolveHost(in); err == nil || !strings.Contains(err.Error(), "attach a project folder instead") {
			t.Errorf("resolveHost(%q) = %v, want refused", in, err)
		}
	}
	if got, err := g.resolveHost("./app"); err != nil || got != filepath.Join(home, "code", "app") {
		t.Errorf("a project folder: %q, %v", got, err)
	}
}
