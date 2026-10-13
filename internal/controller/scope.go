package controller

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// A prefix token is the owner inside its prefix and nobody outside it: it sees, writes and uses only agent
// computers, Services and credentials whose names start with the prefix, and what those point at (grants and uses)
// must start with it too. Credentials are the exception on the way in: its specs may name any. It never sets
// a seccomp profile or an attached host folder: both widen the sandbox, which is the owner's call alone.

func (k caller) prefix() string {
	if k.token == nil {
		return ""
	}
	return k.token.Prefix
}

func (k caller) inPrefix(name string) bool {
	return k.prefix() != "" && strings.HasPrefix(name, k.prefix())
}

// sees reports whether a Fleet-wide read shows the caller this agent computer or Service.
func (k caller) sees(name string) bool { return k.prefix() == "" || k.inPrefix(name) }

// mayCredential reports whether the caller may set or remove a credential.
func (k caller) mayCredential(name string) bool { return k.owner() || k.inPrefix(name) }

func outOfPrefix(k caller, what string) error {
	return &httpError{status: http.StatusForbidden, body: api.Error{Code: api.ErrOutOfPrefix,
		Message: k.principal + " may not touch " + what + ": it is outside the prefix " + k.prefix(),
		Fix:     "use names that start with " + k.prefix() + ", or the owner token"}}
}

// allowOwnerOrPrefix is for the owner's calls a prefix token may also make on names inside its prefix. param names
// the path value holding the name; "" is a Fleet-wide call whose handler filters by sees.
func allowOwnerOrPrefix(param string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k := callerOf(r)
		switch {
		case k.owner():
		case k.prefix() != "" && (param == "" || k.inPrefix(r.PathValue(param))):
		case k.prefix() != "":
			fail(w, outOfPrefix(k, r.PathValue(param)))
			return
		default:
			writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden, Message: k.principal + " may not make this call",
				Fix: "use the owner token, or a prefix token (a12s token create <name> --prefix <p>)"})
			return
		}
		h(w, r)
	}
}

// checkScope keeps a prefix token's write inside its prefix, defaults included: everything the written agent
// computers and Services send to or use must be inside it, and an agent computer it writes (rather than
// removes) keeps the security.seccomp and attach it had before.
func checkScope(k caller, f *fleet.Fleet, before, after *fleet.Spec, o writeOpts) error {
	if k.prefix() == "" {
		return nil
	}
	if o.whole {
		return outOfPrefix(k, "the whole Fleet")
	}
	for _, name := range o.only {
		if !k.inPrefix(name) {
			return outOfPrefix(k, name)
		}
		if _, kept := after.AgentComputers[name]; kept && ownSeccomp(before, name) != ownSeccomp(after, name) {
			return &httpError{status: http.StatusForbidden, body: api.Error{Code: api.ErrForbidden,
				Message: k.principal + " may not set " + name + "'s security.seccomp: only the owner sets a seccomp profile",
				Fix:     "leave security.seccomp as it is, or use the owner token"}}
		}
		if _, kept := after.AgentComputers[name]; kept && ownAttach(before, name) != ownAttach(after, name) {
			return &httpError{status: http.StatusForbidden, body: api.Error{Code: api.ErrForbidden,
				Message: k.principal + " may not set or change " + name + "'s attach: only the owner attaches a host folder",
				Fix:     "leave attach as it is, or use the owner token"}}
		}
		var send []string
		var uses []fleet.Use
		if comp, ok := f.Computers[name]; ok {
			send, uses = comp.Grants.Send, comp.Uses
		}
		if svc, ok := f.Services[name]; ok {
			send, uses = svc.Grants.Send, svc.Uses
		}
		for _, to := range send {
			if !k.inPrefix(to) {
				return outOfPrefix(k, name+"'s grants.send "+to+" (set here or in defaults)")
			}
		}
		for _, u := range uses {
			if !k.inPrefix(u.Name) {
				return outOfPrefix(k, name+"'s uses "+u.Name+" (set here or in defaults)")
			}
		}
	}
	return nil
}

// ownSeccomp identifies the security.seccomp an agent computer sets itself, not through defaults; "" when none.
func ownSeccomp(s *fleet.Spec, name string) string {
	if s == nil {
		return ""
	}
	var c fleet.Computer
	if raw, ok := s.AgentComputers[name]; !ok || json.Unmarshal(raw, &c) != nil {
		return ""
	}
	return c.Security.Seccomp.Path + c.Security.Seccomp.Digest()
}

// ownAttach identifies the attach an agent computer sets; "" when none. Defaults can't carry one.
func ownAttach(s *fleet.Spec, name string) string {
	if s == nil {
		return ""
	}
	var c fleet.Computer
	if raw, ok := s.AgentComputers[name]; !ok || json.Unmarshal(raw, &c) != nil || len(c.Attach) == 0 {
		return ""
	}
	b, _ := json.Marshal(c.Attach)
	return string(b)
}

// filterStatus keeps what a prefix token may see; Fleet-wide warnings and orphans name others, so they go.
func filterStatus(k caller, st api.StatusResponse) api.StatusResponse {
	if k.prefix() == "" {
		return st
	}
	keep := []api.ComputerStatus{}
	for _, cs := range st.AgentComputers {
		if k.inPrefix(cs.Name) {
			keep = append(keep, cs)
		}
	}
	svcs := []api.ServiceStatus{}
	for _, s := range st.Services {
		if k.inPrefix(s.Name) {
			svcs = append(svcs, scopeService(k, s))
		}
	}
	st.AgentComputers, st.Services = keep, svcs
	st.OrphanedDrives, st.OrphanedInboxes, st.Warnings = nil, nil, nil
	return st
}

// scopeService drops what a Service's status says about names outside the caller's prefix.
func scopeService(k caller, s api.ServiceStatus) api.ServiceStatus {
	if k.prefix() == "" {
		return s
	}
	s.UsedBy = slices.DeleteFunc(slices.Clone(s.UsedBy), func(n string) bool { return !k.inPrefix(n) })
	return s
}
