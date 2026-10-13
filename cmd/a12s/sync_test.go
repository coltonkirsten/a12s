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
	long := strings.Repeat("Be thorough. ", 200)
	live := &fleet.Spec{
		APIVersion: fleet.APIVersion, Kind: fleet.Kind, Metadata: fleet.Metadata{Name: "t"},
		Defaults: json.RawMessage(`{"harness":{"kind":"codex","version":"0.154.0"},"image":"slim","credentials":["openai"],"egress":{"allow":[]}}`),
		AgentComputers: map[string]json.RawMessage{
			"short": json.RawMessage(`{"instructions":"Be brief."}`),
			"long":  json.RawMessage(`{"instructions":` + strings.TrimSpace(mustJSON(long)) + `,"files":{"skills/x/SKILL.md":"skill","bin/run":"#!/bin/sh\necho hi\n"}}`),
			"sec":   json.RawMessage(`{"security":{"seccomp":{"defaultAction":"SCMP_ACT_ALLOW"}}}`),
		},
	}
	files, err := exportFiles(live)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["presets/long/AGENTS.md"]; !ok {
		t.Fatalf("long instructions of a codex agent computer go to AGENTS.md: %v", keys(files))
	}
	if strings.Contains(string(files["fleet.yaml"]), "presets/short") {
		t.Fatalf("short instructions stay inline:\n%s", files["fleet.yaml"])
	}
	dir := t.TempDir()
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
