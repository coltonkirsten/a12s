package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

const deploySkill = "---\nname: deploy\ndescription: How to deploy the app\n---\nRun make deploy.\n"

func skillsOf(t *testing.T, res *http.Response) api.SkillsResponse {
	t.Helper()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET skills: %d %+v", res.StatusCode, apiError(t, res))
	}
	var out api.SkillsResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSkillsFleetAndAgentStore(t *testing.T) {
	h := newHarness(t)
	shared := map[string]string{"SKILL.md": "---\nname: style\ndescription: House style\n---\nShort sentences.\n"}
	h.up(labFleet(t, func(f *fleet.Fleet) {
		b := f.Computers["builder"]
		b.Skills = map[string]map[string]string{"style": shared}
		f.Computers["builder"] = b
	}), true)

	empty := skillsOf(t, h.api("GET", "/v1/agent-computers/builder/skills", ""))
	if empty.StoreID != "" || len(empty.Skills) != 1 || empty.Skills[0].Source != api.SkillFromFleet {
		t.Fatalf("before any save: %+v", empty)
	}

	body := `{"files":{"SKILL.md":` + jsonString(deploySkill) + `,"scripts/check.sh":"#!/bin/sh\nmake check\n"}}`
	if res := h.api("PUT", "/v1/agent-computers/builder/skills/deploy", body); res.StatusCode != http.StatusOK {
		t.Fatalf("put: %d %+v", res.StatusCode, apiError(t, res))
	}
	got := skillsOf(t, h.api("GET", "/v1/agent-computers/builder/skills", ""))
	if got.StoreID == "" || len(got.Skills) != 2 || got.Skills[0].Name != "deploy" || got.Skills[0].Source != api.SkillFromAgent || got.Skills[0].By != "local" {
		t.Fatalf("after save: %+v", got)
	}
	if res := h.api("GET", "/v1/agent-computers/builder/skills", "", "If-None-Match", `"`+got.Digest+`"`); res.StatusCode != http.StatusNotModified {
		t.Fatalf("unchanged set: %d", res.StatusCode)
	}

	// A Fleet write never touches the agent's own skills, and they outlive a Controller restart.
	h.up(labFleet(t, nil), true)
	h = h.restart()
	again := skillsOf(t, h.api("GET", "/v1/agent-computers/builder/skills", ""))
	if again.StoreID != got.StoreID || len(again.Skills) != 1 || again.Skills[0].Name != "deploy" {
		t.Fatalf("after a Fleet write and restart: %+v", again)
	}

	if res := h.api("DELETE", "/v1/agent-computers/builder/skills/deploy", ""); res.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", res.StatusCode)
	}
	if res := h.api("DELETE", "/v1/agent-computers/builder/skills/deploy", ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: %d", res.StatusCode)
	}
	if after := skillsOf(t, h.api("GET", "/v1/agent-computers/builder/skills", "")); after.StoreID != got.StoreID || len(after.Skills) != 0 {
		t.Fatalf("an emptied store keeps its id: %+v", after)
	}
}

func TestSkillsRefuseFleetNamesAndBadSkills(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, func(f *fleet.Fleet) {
		f.Defaults.Skills = map[string]map[string]string{"deploy": {"SKILL.md": deploySkill}}
	}), true)
	cases := []struct {
		path, body string
		status     int
	}{
		{"/v1/agent-computers/builder/skills/deploy", `{"files":{"SKILL.md":` + jsonString(deploySkill) + `}}`, http.StatusConflict},
		{"/v1/agent-computers/builder/skills/notes", `{"files":{"README.md":"x"}}`, http.StatusBadRequest},
		{"/v1/agent-computers/builder/skills/notes", `{"files":{"SKILL.md":"no frontmatter"}}`, http.StatusBadRequest},
		{"/v1/agent-computers/builder/skills/notes", `{"files":{"SKILL.md":"---\nname: other\ndescription: x\n---\n"}}`, http.StatusBadRequest},
		{"/v1/agent-computers/builder/skills/Bad_Name", `{"files":{"SKILL.md":"---\ndescription: x\n---\n"}}`, http.StatusBadRequest},
		{"/v1/agent-computers/builder/skills/notes", `{"files":{"SKILL.md":"---\ndescription: x\n---\n","../escape":"x"}}`, http.StatusBadRequest},
		{"/v1/agent-computers/builder/skills/notes", `{"files":{"SKILL.md":"---\ndescription: x\n---\n",".":"x"}}`, http.StatusBadRequest},
		{"/v1/agent-computers/builder/skills/notes", `{"files":{"SKILL.md":"---\ndescription: x\n---\n","ref":"x","ref/a.md":"y"}}`, http.StatusBadRequest},
		{"/v1/agent-computers/nobody/skills/notes", `{"files":{"SKILL.md":"---\ndescription: x\n---\n"}}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		if res := h.api("PUT", tc.path, tc.body); res.StatusCode != tc.status {
			t.Errorf("PUT %s %s: %d, want %d: %+v", tc.path, tc.body, res.StatusCode, tc.status, apiError(t, res))
		}
	}
	if res := h.api("DELETE", "/v1/agent-computers/builder/skills/deploy", ""); res.StatusCode != http.StatusConflict {
		t.Fatalf("delete a Fleet skill: %d", res.StatusCode)
	}
}

func TestSkillsGoWithTheAgentComputer(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), true)
	if res := h.api("PUT", "/v1/agent-computers/builder/skills/deploy", `{"files":{"SKILL.md":`+jsonString(deploySkill)+`}}`); res.StatusCode != http.StatusOK {
		t.Fatalf("put: %d", res.StatusCode)
	}
	first := skillsOf(t, h.api("GET", "/v1/agent-computers/builder/skills", ""))
	h.up(labFleet(t, func(f *fleet.Fleet) { delete(f.Computers, "builder") }), true)
	h.up(labFleet(t, nil), true)
	back := skillsOf(t, h.api("GET", "/v1/agent-computers/builder/skills", ""))
	if back.StoreID != "" || len(back.Skills) != 0 || first.StoreID == "" {
		t.Fatalf("re-added agent computer: %+v (was %+v)", back, first)
	}
}

func TestSkillsOverMTLSAreTheAgentsOwn(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	h.waitPhase("builder", api.PhaseReady)
	srv := mtlsServer(t, h)
	leaf, _ := h.c.ca.ComputerCert("builder", 1)
	if err := h.c.registerCert(context.Background(), Principal{Kind: KindComputer, Name: "builder", Generation: 1}, leaf.Serial, nil); err != nil {
		t.Fatal(err)
	}
	body := `{"files":{"SKILL.md":` + jsonString(deploySkill) + `}}`
	if code, e := mtlsDo(t, srv, h.c.ca, leaf, "PUT", "/v1/agent-computers/builder/skills/deploy", body); code != http.StatusOK {
		t.Fatalf("own skill: %d %+v", code, e)
	}
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "PUT", "/v1/agent-computers/scout/skills/deploy", body); code != http.StatusForbidden {
		t.Fatalf("another computer's skill: %d", code)
	}
	if code, _ := mtlsDo(t, srv, h.c.ca, leaf, "GET", "/v1/agent-computers/scout/skills", ""); code != http.StatusForbidden {
		t.Fatalf("read another computer's skills: %d", code)
	}
	got := skillsOf(t, h.api("GET", "/v1/agent-computers/builder/skills", ""))
	if len(got.Skills) != 1 || got.Skills[0].By != "computer:builder" {
		t.Fatalf("saved by the agent: %+v", got)
	}
}
