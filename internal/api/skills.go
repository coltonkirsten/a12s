package api

import "time"

// Where a skill comes from: the Fleet (agent_computers.<name>.skills or defaults.skills) or the agent's own store.
const (
	SkillFromFleet = "fleet"
	SkillFromAgent = "agent"
)

// Skill is one harness skill: its files by path inside the skill folder, SKILL.md among them.
type Skill struct {
	Name   string            `json:"name"`
	Source string            `json:"source"`
	Files  map[string]string `json:"files"`
	// UpdatedAt and By are set for a skill from the agent's store: when and by whom it was last saved.
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	By        string     `json:"by,omitempty"`
}

// SkillsResponse is GET /v1/agent-computers/{c}/skills: every skill the agent computer's harness sees, by name.
type SkillsResponse struct {
	// StoreID names the agent's skill store; it changes only when the store is created anew, so a caller that keeps
	// a copy can tell a skill the agent deleted from a store that was lost.
	StoreID string  `json:"storeId"`
	Digest  string  `json:"digest"`
	Skills  []Skill `json:"skills"`
}

// PutSkillRequest is PUT /v1/agent-computers/{c}/skills/{name}.
type PutSkillRequest struct {
	Files map[string]string `json:"files"`
}
