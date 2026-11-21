package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	// SkillFile is the file every skill has: YAML frontmatter with a description, then the instructions.
	SkillFile = "SKILL.md"
	// MaxSkillsBytes bounds the skills of one agent computer in its spec, on top of MaxFilesBytes.
	MaxSkillsBytes = 1 << 20
	// MaxAgentSkillBytes, MaxAgentSkillsBytes and MaxAgentSkills bound the skills an agent saves for itself.
	MaxAgentSkillBytes  = 512 << 10
	MaxAgentSkillsBytes = 4 << 20
	MaxAgentSkills      = 200
)

var skillNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// SkillSize is what a skill counts against a budget: its paths and contents.
func SkillSize(files map[string]string) int {
	n := 0
	for p, body := range files {
		n += len(p) + len(body)
	}
	return n
}

// ValidateSkill checks one skill's name and files; each problem is one message.
func ValidateSkill(name string, files map[string]string) []string {
	var bad []string
	if !skillNameRE.MatchString(name) {
		bad = append(bad, fmt.Sprintf("%q must be lowercase letters, digits and hyphens, at most 64 characters", name))
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		if p == "" || p == "." || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			bad = append(bad, fmt.Sprintf("%q must be a clean path inside the skill folder", p))
		}
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if _, ok := files[d]; ok {
				bad = append(bad, fmt.Sprintf("%q is a file and also the folder of %q", d, p))
			}
		}
	}
	body, ok := files[SkillFile]
	if !ok {
		return append(bad, "needs a "+SkillFile)
	}
	front, ok := frontmatter(body)
	switch {
	case !ok:
		bad = append(bad, SkillFile+" must start with YAML frontmatter between --- lines, with a description")
	case front["description"] == "":
		bad = append(bad, SkillFile+" frontmatter needs a description: it is how the agent knows when to use the skill")
	case front["name"] != "" && front["name"] != name:
		bad = append(bad, fmt.Sprintf("%s frontmatter names it %q; it must match the skill name %q", SkillFile, front["name"], name))
	}
	return bad
}

// frontmatter reads the top-level scalar keys of a leading --- block, enough to check name and description.
func frontmatter(body string) (map[string]string, bool) {
	body = strings.TrimPrefix(body, "\uFEFF")
	rest, ok := strings.CutPrefix(body, "---\n")
	if !ok {
		if rest, ok = strings.CutPrefix(body, "---\r\n"); !ok {
			return nil, false
		}
	}
	out := map[string]string{}
	for line := range strings.Lines(rest) {
		line = strings.TrimRight(line, "\r\n")
		if line == "---" {
			return out, true
		}
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if v == "|" || v == ">" || v == "|-" || v == ">-" {
			v = "(block)"
		}
		out[strings.TrimSpace(k)] = v
	}
	return nil, false
}

func validateSkills(skills map[string]map[string]string) []string {
	var bad []string
	total := 0
	for _, name := range slices.Sorted(maps.Keys(skills)) {
		total += len(name) + SkillSize(skills[name])
		for _, msg := range ValidateSkill(name, skills[name]) {
			bad = append(bad, fmt.Sprintf("skills.%s: %s", name, msg))
		}
	}
	if total > MaxSkillsBytes {
		bad = append(bad, fmt.Sprintf("skills: %d bytes, more than 1 MiB", total))
	}
	return bad
}

// SkillsDigest names a set of skills: the same skills always give the same digest.
func SkillsDigest(skills map[string]map[string]string) string {
	b, _ := json.Marshal(skills)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
