package fleet

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const specYAML = `apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: t }
defaults:
  harness: { kind: claude-code, version: 2.1.280, model: haiku }
  image: slim
  credentials: [anthropic]
  files: { skills/a/SKILL.md: shared }
KEY:
  a: { harness: { model: sonnet }, files: { notes.md: mine } }
  b: { credentials: [] }
`

// A spec keeps each agent computer as written; Effective merges the defaults in: maps merge, credentials add to the
// defaults' (an explicit empty list too).
func TestSpecMergesDefaults(t *testing.T) {
	s, err := DecodeSpec([]byte(strings.Replace(specYAML, "KEY", "agent_computers", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(s.AgentComputers["b"]); got != `{"credentials":[]}` {
		t.Errorf("b as written = %s", got)
	}
	f, err := s.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	a, b := f.Computers["a"], f.Computers["b"]
	if !reflect.DeepEqual(a.Harness, Harness{Kind: HarnessClaudeCode, Version: "2.1.280", Model: "sonnet"}) || len(a.Files) != 2 || a.Credentials[0] != "anthropic" {
		t.Errorf("a = %+v", a)
	}
	if !slices.Equal(b.Credentials, []string{"anthropic"}) || b.Image != "slim" {
		t.Errorf("b = %+v", b)
	}
	if _, err := DecodeSpec([]byte(strings.Replace(specYAML, "KEY", "computers", 1))); err == nil {
		t.Error("the key computers must be refused")
	}
}

func TestMergePatch(t *testing.T) {
	got, err := MergePatch(json.RawMessage(`{"a":1,"m":{"x":1,"y":2},"l":[1]}`), json.RawMessage(`{"a":null,"m":{"y":3},"l":[2]}`))
	if err != nil || Canonical(got) != `{"l":[2],"m":{"x":1,"y":3}}` {
		t.Fatalf("MergePatch = %s, %v", got, err)
	}
}

// A grant to an agent computer that doesn't exist yet is a warning, so a pair can be created one at a time.
func TestDanglingGrantWarns(t *testing.T) {
	y := strings.Replace(strings.Replace(specYAML, "KEY", "agent_computers", 1), "b: { credentials: [] }", "b: { credentials: [], grants: { send: [later] } }", 1)
	f, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(f.GrantWarnings(), "\n"); !strings.Contains(w, "agent_computers.b.grants.send: there is no agent computer later") {
		t.Fatalf("warnings = %q", w)
	}
}
