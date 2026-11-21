package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// An agent's own skills live beside state.json, one file per agent computer, so the Fleet's writers (a12s up, an app
// that PUTs its specs) never overwrite them and they never count against a spec's size.
const skillsDir = "skills"

type skillStore struct {
	ID     string                 `json:"id"`
	Skills map[string]storedSkill `json:"skills"`
}

type storedSkill struct {
	Files     map[string]string `json:"files"`
	UpdatedAt time.Time         `json:"updatedAt"`
	By        string            `json:"by"`
}

func (c *Controller) skillStorePath(name string) string {
	return filepath.Join(c.cfg.StateDir, skillsDir, name+".json")
}

// loadSkillStore returns an empty store with no ID when the agent computer has none yet.
func (c *Controller) loadSkillStore(name string) (skillStore, error) {
	st := skillStore{Skills: map[string]storedSkill{}}
	b, err := os.ReadFile(c.skillStorePath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %w", c.skillStorePath(name), err)
	}
	if st.Skills == nil {
		st.Skills = map[string]storedSkill{}
	}
	return st, nil
}

func (c *Controller) saveSkillStore(name string, st skillStore) error {
	if err := os.MkdirAll(filepath.Join(c.cfg.StateDir, skillsDir), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.skillStorePath(name), append(b, '\n'), 0o600)
}

func (c *Controller) removeSkillStore(name string) error {
	c.skillsMu.Lock()
	defer c.skillsMu.Unlock()
	if err := os.Remove(c.skillStorePath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// fleetSkills is the agent computer's skills from the applied Fleet, defaults merged in.
func (c *Controller) fleetSkills(name string) (map[string]map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.Fleet == nil {
		return nil, notFound("agent computer", name)
	}
	comp, ok := c.state.Fleet.Computers[name]
	if !ok {
		return nil, notFound("agent computer", name)
	}
	return comp.Skills, nil
}

// skills is every skill the agent computer's harness sees: the Fleet's, then the agent's own under other names.
func (c *Controller) skills(name string) (api.SkillsResponse, error) {
	fromFleet, err := c.fleetSkills(name)
	if err != nil {
		return api.SkillsResponse{}, err
	}
	c.skillsMu.Lock()
	st, err := c.loadSkillStore(name)
	c.skillsMu.Unlock()
	if err != nil {
		return api.SkillsResponse{}, err
	}
	res := api.SkillsResponse{StoreID: st.ID, Skills: []api.Skill{}}
	all := map[string]map[string]string{}
	for _, n := range slices.Sorted(maps.Keys(fromFleet)) {
		res.Skills = append(res.Skills, api.Skill{Name: n, Source: api.SkillFromFleet, Files: fromFleet[n]})
		all[n] = fromFleet[n]
	}
	for _, n := range slices.Sorted(maps.Keys(st.Skills)) {
		if _, shadowed := fromFleet[n]; shadowed {
			continue
		}
		s := st.Skills[n]
		at := s.UpdatedAt
		res.Skills = append(res.Skills, api.Skill{Name: n, Source: api.SkillFromAgent, Files: s.Files, UpdatedAt: &at, By: s.By})
		all[n] = s.Files
	}
	slices.SortFunc(res.Skills, func(a, b api.Skill) int { return strings.Compare(a.Name, b.Name) })
	res.Digest = fleet.SkillsDigest(all)
	return res, nil
}

func fleetOwned(name, skill string) error {
	return &httpError{status: http.StatusConflict, body: api.Error{Code: api.ErrConflict,
		Message: fmt.Sprintf("skill %q of %s comes from the Fleet", skill, name),
		Fix:     "save it under another name, or change agent_computers." + name + ".skills (or defaults.skills) in the Fleet"}}
}

func (c *Controller) putSkill(name, skill string, files map[string]string, by string) (api.Skill, error) {
	if msgs := fleet.ValidateSkill(skill, files); len(msgs) > 0 {
		return api.Skill{}, errorf(http.StatusBadRequest, api.ErrInvalid, "skill %s: %s", skill, strings.Join(msgs, "; "))
	}
	if size := fleet.SkillSize(files); size > fleet.MaxAgentSkillBytes {
		return api.Skill{}, errorf(http.StatusRequestEntityTooLarge, api.ErrBodyTooLarge, "skill %s is %d bytes, more than %d KiB", skill, size, fleet.MaxAgentSkillBytes>>10)
	}
	fromFleet, err := c.fleetSkills(name)
	if err != nil {
		return api.Skill{}, err
	}
	if _, ok := fromFleet[skill]; ok {
		return api.Skill{}, fleetOwned(name, skill)
	}
	c.skillsMu.Lock()
	defer c.skillsMu.Unlock()
	// Checked again under the lock: a removal that ran since must not leave a store behind for a later namesake.
	if _, err := c.fleetSkills(name); err != nil {
		return api.Skill{}, err
	}
	st, err := c.loadSkillStore(name)
	if err != nil {
		return api.Skill{}, err
	}
	if st.ID == "" {
		st.ID = newStoreID()
	}
	if _, ok := st.Skills[skill]; !ok && len(st.Skills) >= fleet.MaxAgentSkills {
		return api.Skill{}, errorf(http.StatusRequestEntityTooLarge, api.ErrBodyTooLarge, "%s already has %d skills; delete one first", name, fleet.MaxAgentSkills)
	}
	now := c.now().UTC()
	st.Skills[skill] = storedSkill{Files: files, UpdatedAt: now, By: by}
	total := 0
	for n, s := range st.Skills {
		total += len(n) + fleet.SkillSize(s.Files)
	}
	if total > fleet.MaxAgentSkillsBytes {
		return api.Skill{}, errorf(http.StatusRequestEntityTooLarge, api.ErrBodyTooLarge, "%s's skills would be %d bytes, more than %d MiB; delete or shorten some", name, total, fleet.MaxAgentSkillsBytes>>20)
	}
	if err := c.saveSkillStore(name, st); err != nil {
		return api.Skill{}, err
	}
	c.logf("%s: skill %s saved by %s", name, skill, by)
	return api.Skill{Name: skill, Source: api.SkillFromAgent, Files: files, UpdatedAt: &now, By: by}, nil
}

func (c *Controller) deleteSkill(name, skill, by string) error {
	fromFleet, err := c.fleetSkills(name)
	if err != nil {
		return err
	}
	if _, ok := fromFleet[skill]; ok {
		return fleetOwned(name, skill)
	}
	c.skillsMu.Lock()
	defer c.skillsMu.Unlock()
	st, err := c.loadSkillStore(name)
	if err != nil {
		return err
	}
	if _, ok := st.Skills[skill]; !ok {
		return &httpError{status: http.StatusNotFound, body: api.Error{Code: api.ErrNotFound, Message: fmt.Sprintf("%s has no skill %q", name, skill)}}
	}
	delete(st.Skills, skill)
	if err := c.saveSkillStore(name, st); err != nil {
		return err
	}
	c.logf("%s: skill %s deleted by %s", name, skill, by)
	return nil
}

func newStoreID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Controller) handleSkills(w http.ResponseWriter, r *http.Request) {
	res, err := c.skills(r.PathValue("c"))
	if err != nil {
		fail(w, err)
		return
	}
	tag := `"` + res.Digest + `"`
	w.Header().Set("ETag", tag)
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (c *Controller) handlePutSkill(w http.ResponseWriter, r *http.Request) {
	var req api.PutSkillRequest
	if !decode(w, r, maxSpecBody, &req) {
		return
	}
	s, err := c.putSkill(r.PathValue("c"), r.PathValue("skill"), req.Files, skillAuthor(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (c *Controller) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	if err := c.deleteSkill(r.PathValue("c"), r.PathValue("skill"), skillAuthor(r)); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("skill")})
}

func skillAuthor(r *http.Request) string {
	if p, ok := peerOf(r); ok {
		return p.String()
	}
	return callerOf(r).principal
}

// ownSkills admits an agent computer's calls on its own skills only.
func ownSkills(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := peerOf(r)
		if p.Kind != KindComputer || p.Name != r.PathValue("c") {
			writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden,
				Message: fmt.Sprintf("%s may not touch the skills of %s", p, r.PathValue("c")),
				Fix:     "an agent computer reads and writes only its own skills"})
			return
		}
		h(w, r)
	}
}
