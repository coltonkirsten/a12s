package controller

import (
	"context"
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

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// credsFile holds credential values in the state volume, mode 0600: anyone who can read it can already exec into
// an agent computer and read /run/a12s/secrets, and without it every restart would break the Fleet's apps.
const credsFile = "credentials.json"

func (c *Controller) loadCreds() error {
	p := filepath.Join(c.cfg.StateDir, credsFile)
	if c.cfg.MemoryOnlyCredentials {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	var m api.Credentials
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("load credentials %s: %w", p, err)
	}
	c.creds = m
	return nil
}

// setCredsLocked merges values into the store and persists it; nil values delete.
func (c *Controller) setCredsLocked(values api.Credentials) error {
	for k, v := range values {
		if v == nil {
			delete(c.creds, k)
		} else {
			c.creds[k] = v
		}
	}
	if c.cfg.MemoryOnlyCredentials {
		return nil
	}
	data, err := json.Marshal(c.creds)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(c.cfg.StateDir, credsFile), data, 0o600)
}

// credentialUsers names the agent computers and Services that use a credential.
func (c *Controller) credentialUsersLocked(name string) []string {
	var out []string
	if f := c.state.Fleet; f != nil {
		for _, n := range f.ComputerNames() {
			if slices.Contains(f.Computers[n].Credentials, name) {
				out = append(out, n)
			}
		}
		for _, n := range f.ServiceNames() {
			if slices.Contains(f.Services[n].Credentials, name) {
				out = append(out, n)
			}
		}
	}
	return out
}

func (c *Controller) handleCredentials(w http.ResponseWriter, r *http.Request) {
	k := callerOf(r)
	c.mu.Lock()
	names := map[string]bool{}
	for n := range c.creds {
		names[n] = true
	}
	if f := c.state.Fleet; f != nil {
		for _, comp := range f.Computers {
			for _, n := range comp.Credentials {
				names[n] = true
			}
		}
		for _, s := range f.Services {
			for _, n := range s.Credentials {
				names[n] = true
			}
		}
	}
	out := []api.CredentialInfo{}
	for _, n := range slices.Sorted(maps.Keys(names)) {
		if !k.mayCredential(n) {
			continue
		}
		users := slices.DeleteFunc(c.credentialUsersLocked(n), func(u string) bool { return !k.sees(u) })
		out = append(out, api.CredentialInfo{Name: n, Set: len(c.creds[n]) > 0, UsedBy: users})
	}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, api.CredentialsResponse{Credentials: out})
}

func (c *Controller) handlePutCredential(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !fleet.ValidCredential(name) {
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "credential name "+name+" must be lowercase letters, digits, - and _")
		return
	}
	if !callerOf(r).mayCredential(name) {
		fail(w, outOfPrefix(callerOf(r), "credential "+name))
		return
	}
	var req api.CredentialRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	if req.Value == "" {
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "value is required")
		return
	}
	c.mu.Lock()
	err := c.setCredsLocked(api.Credentials{name: []byte(req.Value)})
	users := c.credentialUsersLocked(name)
	c.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	c.logf("credential %s set by %s", name, callerOf(r).principal)
	if len(users) > 0 {
		// The users pick up the new value: running ones get it injected, ones that waited for it start.
		_, err, ok := detached(c, r, func(ctx context.Context) (api.PutFleetResponse, error) {
			return c.applySpec(ctx, func(s *fleet.Spec) (*fleet.Spec, error) { return s, nil }, writeOpts{only: users, by: callerOf(r)})
		})
		if !ok {
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, api.CredentialInfo{Name: name, Set: true, UsedBy: users})
}

func (c *Controller) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !callerOf(r).mayCredential(name) {
		fail(w, outOfPrefix(callerOf(r), "credential "+name))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.creds[name]; !ok {
		fail(w, notFound("credential", name))
		return
	}
	if users := c.credentialUsersLocked(name); len(users) > 0 {
		fail(w, &httpError{status: http.StatusConflict, body: api.Error{Code: api.ErrInUse,
			Message: fmt.Sprintf("credential %s is used by %s", name, strings.Join(users, ", ")),
			Fix:     "remove it from their credentials first"}})
		return
	}
	if err := c.setCredsLocked(api.Credentials{name: nil}); err != nil {
		fail(w, err)
		return
	}
	c.logf("credential %s removed by %s", name, callerOf(r).principal)
	w.WriteHeader(http.StatusNoContent)
}
