package fleet

import (
	"encoding/json"
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

// A spec keeps each agent computer as written; Effective merges the defaults in: maps merge, lists replace (an
// explicit empty one too). The old computers key still parses.
func TestSpecMergesDefaults(t *testing.T) {
	for _, key := range []string{"agent_computers", "computers"} {
		s, err := DecodeSpec([]byte(strings.Replace(specYAML, "KEY", key, 1)))
		if err != nil {
			t.Fatal(err)
		}
		if s.LegacyKey != (key == "computers") {
			t.Errorf("%s: LegacyKey = %v", key, s.LegacyKey)
		}
		if got := string(s.AgentComputers["b"]); got != `{"credentials":[]}` {
			t.Errorf("b as written = %s", got)
		}
		f, err := s.Resolve()
		if err != nil {
			t.Fatal(err)
		}
		a, b := f.Computers["a"], f.Computers["b"]
		if a.Harness != (Harness{Kind: HarnessClaudeCode, Version: "2.1.280", Model: "sonnet"}) || len(a.Files) != 2 || a.Credentials[0] != "anthropic" {
			t.Errorf("a = %+v", a)
		}
		if len(b.Credentials) != 0 || b.Image != "slim" {
			t.Errorf("b = %+v", b)
		}
	}
	both := strings.Replace(specYAML, "KEY", "agent_computers", 1) + "computers: { c: {} }\n"
	if _, err := DecodeSpec([]byte(both)); err == nil {
		t.Error("both keys at once must be refused")
	}
}

func TestMergePatch(t *testing.T) {
	got, err := MergePatch(json.RawMessage(`{"a":1,"m":{"x":1,"y":2},"l":[1]}`), json.RawMessage(`{"a":null,"m":{"y":3},"l":[2]}`))
	if err != nil || Canonical(got) != `{"l":[2],"m":{"x":1,"y":3}}` {
		t.Fatalf("MergePatch = %s, %v", got, err)
	}
}
