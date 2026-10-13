package controller

import (
	"net/http"
	"slices"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// A prefix token is the owner inside its prefix and nobody outside it: it sees, writes and uses only agent
// computers, Services and credentials whose names start with the prefix, and what those point at (grants, uses,
// drives) must start with it too. Credentials are the exception on the way in: its specs may name any.

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
// computers and Services send to, use or attach must be inside it.
func checkScope(k caller, f *fleet.Fleet, o writeOpts) error {
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
		var send []string
		var uses []fleet.Use
		if comp, ok := f.Computers[name]; ok {
			send, uses = comp.Grants.Send, comp.Uses
			for _, d := range comp.Drives {
				if !k.inPrefix(d.Name) {
					return outOfPrefix(k, name+"'s drive "+d.Name)
				}
			}
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
	st.AgentComputers, st.Computers, st.Services = keep, keep, svcs
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
