package fleet

import (
	"encoding/json"
	"testing"
)

func TestSkillsReplaceDefaultsByName(t *testing.T) {
	s := &Spec{APIVersion: APIVersion, Kind: Kind, Metadata: Metadata{Name: "lab"},
		Defaults: json.RawMessage(`{"skills":{"deploy":{"SKILL.md":"d","old.sh":"x"},"style":{"SKILL.md":"s"}}}`),
		AgentComputers: map[string]json.RawMessage{
			"a": json.RawMessage(`{"skills":{"deploy":{"SKILL.md":"mine"},"style":null}}`),
			"b": json.RawMessage(`{}`),
		}}
	f, err := s.Effective()
	if err != nil {
		t.Fatal(err)
	}
	a, b := f.Computers["a"].Skills, f.Computers["b"].Skills
	if len(a) != 1 || len(a["deploy"]) != 1 || a["deploy"]["SKILL.md"] != "mine" {
		t.Fatalf("an agent computer's skill replaces the default whole, null drops one: %v", a)
	}
	if len(b) != 2 || b["deploy"]["old.sh"] != "x" {
		t.Fatalf("defaults: %v", b)
	}
}
