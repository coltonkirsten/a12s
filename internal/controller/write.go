package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// maxSpecBody bounds one agent computer's or Service's spec: up to 1 MiB of files, JSON-escaped.
const maxSpecBody = 8 << 20

// legacyPaths serves the /v1/computers/... paths of earlier releases as /v1/agent-computers/....
func legacyPaths(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v1/computers"); ok && (rest == "" || rest[0] == '/') {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/v1/agent-computers" + rest
			if r.URL.RawPath != "" {
				r2.URL.RawPath = "/v1/agent-computers" + strings.TrimPrefix(r.URL.RawPath, "/v1/computers")
			}
			r = r2
		}
		next.ServeHTTP(w, r)
	})
}

func dryRun(r *http.Request) bool { return r.URL.Query().Get("dryRun") == "true" }

// ifMatch reads the version an If-Match header names; "*" or none is unconditional.
func ifMatch(r *http.Request) (*int64, error) {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" || h == "*" {
		return nil, nil
	}
	v, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(h, "W/"), `"`), 10, 64)
	if err != nil {
		return nil, errorf(http.StatusBadRequest, api.ErrInvalid, "If-Match %q is not a Fleet version", h)
	}
	return &v, nil
}

func etag(v int64) string { return `"` + strconv.FormatInt(v, 10) + `"` }

func (c *Controller) write(w http.ResponseWriter, r *http.Request, mutate func(*fleet.Spec) (*fleet.Spec, error), o writeOpts, view func(ctx context.Context, res *api.PutFleetResponse)) {
	o.dryRun, o.by = dryRun(r), callerOf(r)
	res, err, ok := detached(c, r, func(ctx context.Context) (api.PutFleetResponse, error) {
		res, err := c.applySpec(ctx, mutate, o)
		if err == nil && !res.DryRun && view != nil {
			view(ctx, &res)
		}
		return res, err
	})
	if !ok {
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("ETag", etag(res.Version))
	writeJSON(w, http.StatusOK, res)
}

func (c *Controller) handleGetFleet(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	spec, version := c.state.Spec, c.state.Version
	c.mu.Unlock()
	if spec == nil {
		spec = &fleet.Spec{APIVersion: fleet.APIVersion, Kind: fleet.Kind, Metadata: fleet.Metadata{Name: c.cfg.Fleet}, AgentComputers: map[string]json.RawMessage{}}
	}
	res := api.FleetResponse{Version: version, Fleet: *spec}
	w.Header().Set("ETag", etag(version))
	writeJSON(w, http.StatusOK, res)
}

func (c *Controller) handlePutFleet(w http.ResponseWriter, r *http.Request) {
	var req api.PutFleetRequest
	if !decode(w, r, maxFleetBody, &req) {
		return
	}
	match, err := ifMatch(r)
	if err != nil {
		fail(w, err)
		return
	}
	o := writeOpts{ifMatch: match, dryRun: dryRun(r), by: callerOf(r)}
	res, err, ok := detached(c, r, func(ctx context.Context) (api.PutFleetResponse, error) { return c.PutFleet(ctx, req, o) })
	if !ok {
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("ETag", etag(res.Version))
	writeJSON(w, http.StatusOK, res)
}

// readObject reads a JSON object body.
func readObject(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	var raw json.RawMessage
	if !decode(w, r, maxSpecBody, &raw) {
		return nil, false
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 {
		raw = json.RawMessage("{}")
	} else if t[0] != '{' {
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "body: want a JSON object")
		return nil, false
	}
	return raw, true
}

func notFound(kind, name string) error {
	return &httpError{status: http.StatusNotFound, body: api.Error{Code: api.ErrNotFound, Message: fmt.Sprintf("no %s %q in the Fleet", kind, name)}}
}

func exists(kind, name string) error {
	return &httpError{status: http.StatusConflict, body: api.Error{Code: api.ErrExists, Message: fmt.Sprintf("%s %s already exists", kind, name),
		Fix: "get it, or create it with replace=True (the API: leave out If-None-Match)"}}
}

func createOnly(r *http.Request) bool { return strings.TrimSpace(r.Header.Get("If-None-Match")) == "*" }

func (c *Controller) handleAgentComputers(w http.ResponseWriter, r *http.Request) {
	k := callerOf(r)
	c.mu.Lock()
	var names []string
	if c.state.Spec != nil {
		names = c.state.Spec.Names()
	}
	c.mu.Unlock()
	names = slices.DeleteFunc(names, func(n string) bool { return !k.may(api.ActionStatus, n) })
	out := make([]api.AgentComputer, 0, len(names))
	for _, n := range names {
		if v, ok := c.agentComputerView(r.Context(), n); ok {
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, api.AgentComputersResponse{AgentComputers: out})
}

func (c *Controller) handleAgentComputer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	v, ok := c.agentComputerView(r.Context(), name)
	if !ok {
		fail(w, notFound("agent computer", name))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (c *Controller) agentComputerView(ctx context.Context, name string) (api.AgentComputer, bool) {
	c.mu.Lock()
	var raw json.RawMessage
	var comp fleet.Computer
	ok := c.state.Spec != nil && c.state.Fleet != nil
	if ok {
		raw, ok = c.state.Spec.AgentComputers[name]
		comp = c.state.Fleet.Computers[name]
	}
	c.mu.Unlock()
	if !ok {
		return api.AgentComputer{}, false
	}
	return api.AgentComputer{Name: name, Spec: raw, Effective: fleet.SpecOf(comp), Status: c.computerStatus(ctx, name)}, true
}

func (c *Controller) attachAgentComputer(name string) func(context.Context, *api.PutFleetResponse) {
	return func(ctx context.Context, res *api.PutFleetResponse) {
		if v, ok := c.agentComputerView(ctx, name); ok {
			res.AgentComputer = &v
		}
	}
}

func (c *Controller) handlePutAgentComputer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	raw, ok := readObject(w, r)
	if !ok {
		return
	}
	only := createOnly(r)
	c.write(w, r, func(s *fleet.Spec) (*fleet.Spec, error) {
		if _, taken := s.AgentComputers[name]; taken && only {
			return nil, exists("agent computer", name)
		}
		s.AgentComputers[name] = raw
		return s, nil
	}, writeOpts{only: []string{name}}, c.attachAgentComputer(name))
}

func (c *Controller) handlePatchAgentComputer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	patch, ok := readObject(w, r)
	if !ok {
		return
	}
	c.write(w, r, func(s *fleet.Spec) (*fleet.Spec, error) {
		cur, ok := s.AgentComputers[name]
		if !ok {
			return nil, notFound("agent computer", name)
		}
		next, err := fleet.MergePatch(cur, patch)
		if err != nil {
			return nil, errorf(http.StatusBadRequest, api.ErrInvalid, "%v", err)
		}
		s.AgentComputers[name] = next
		return s, nil
	}, writeOpts{only: []string{name}}, c.attachAgentComputer(name))
}

func (c *Controller) handleDeleteAgentComputer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	c.write(w, r, func(s *fleet.Spec) (*fleet.Spec, error) {
		if _, ok := s.AgentComputers[name]; !ok {
			return nil, notFound("agent computer", name)
		}
		delete(s.AgentComputers, name)
		return s, nil
	}, writeOpts{only: []string{name}}, nil)
}

func decodeService(raw json.RawMessage) (fleet.Service, error) {
	var svc fleet.Service
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&svc); err != nil {
		return svc, errorf(http.StatusBadRequest, api.ErrInvalid, "service: %v", err)
	}
	return svc, nil
}

func (c *Controller) attachService(k caller, name string) func(context.Context, *api.PutFleetResponse) {
	return func(_ context.Context, res *api.PutFleetResponse) {
		if c.isService(name) {
			st := scopeService(k, c.serviceStatus(name))
			res.Service = &st
		}
	}
}

func (c *Controller) handlePutService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("s")
	raw, ok := readObject(w, r)
	if !ok {
		return
	}
	only := createOnly(r)
	c.write(w, r, func(s *fleet.Spec) (*fleet.Spec, error) {
		svc, err := decodeService(raw)
		if err != nil {
			return nil, err
		}
		if _, taken := s.Services[name]; taken && only {
			return nil, exists("Service", name)
		}
		if s.Services == nil {
			s.Services = map[string]fleet.Service{}
		}
		s.Services[name] = svc
		return s, nil
	}, writeOpts{only: []string{name}}, c.attachService(callerOf(r), name))
}

func (c *Controller) handlePatchService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("s")
	patch, ok := readObject(w, r)
	if !ok {
		return
	}
	c.write(w, r, func(s *fleet.Spec) (*fleet.Spec, error) {
		cur, ok := s.Services[name]
		if !ok {
			return nil, notFound("Service", name)
		}
		b, err := json.Marshal(cur)
		if err != nil {
			return nil, err
		}
		next, err := fleet.MergePatch(b, patch)
		if err != nil {
			return nil, errorf(http.StatusBadRequest, api.ErrInvalid, "%v", err)
		}
		if s.Services[name], err = decodeService(next); err != nil {
			return nil, err
		}
		return s, nil
	}, writeOpts{only: []string{name}}, c.attachService(callerOf(r), name))
}

func (c *Controller) handleDeleteService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("s")
	c.write(w, r, func(s *fleet.Spec) (*fleet.Spec, error) {
		if _, ok := s.Services[name]; !ok {
			return nil, notFound("Service", name)
		}
		delete(s.Services, name)
		return s, nil
	}, writeOpts{only: []string{name}}, nil)
}

// diff lists what a write changes among the names it may touch.
func (c *Controller) diff(old, cur *fleet.Fleet, o writeOpts) []api.Change {
	var oldC map[string]fleet.Computer
	var oldS map[string]fleet.Service
	if old != nil {
		oldC, oldS = old.Computers, old.Services
	}
	out := []api.Change{}
	out = append(out, diffKind(api.KindAgentComputer, oldC, cur.Computers, o, func(name string) string {
		if c.phase(name) == api.PhaseBusy {
			return api.WhenIdle
		}
		return api.WhenNow
	})...)
	out = append(out, diffKind(api.KindService, oldS, cur.Services, o, func(string) string { return api.WhenNow })...)
	return out
}

func diffKind[T any](kind string, old, cur map[string]T, o writeOpts, when func(string) string) []api.Change {
	names := map[string]bool{}
	for n := range old {
		names[n] = true
	}
	for n := range cur {
		names[n] = true
	}
	var out []api.Change
	for _, n := range slices.Sorted(maps.Keys(names)) {
		if !o.touches(n) {
			continue
		}
		a, hadA := old[n]
		b, hasB := cur[n]
		switch {
		case !hadA:
			out = append(out, api.Change{Op: api.ChangeCreate, Kind: kind, Name: n, When: api.WhenNow})
		case !hasB:
			out = append(out, api.Change{Op: api.ChangeRemove, Kind: kind, Name: n, When: api.WhenNow})
		default:
			if fields, diff := changedFields(a, b); len(fields) > 0 {
				out = append(out, api.Change{Op: api.ChangeUpdate, Kind: kind, Name: n, When: when(n), Fields: fields, Diff: diff})
			}
		}
	}
	return out
}

func changedFields(a, b any) ([]string, []api.FieldDiff) {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	var fields []string
	var diff []api.FieldDiff
	for _, k := range keysOf(ja, jb) {
		n := len(diff)
		diffJSON(k.name, k.a, k.b, &diff)
		if len(diff) > n {
			fields = append(fields, k.name)
		}
	}
	return fields, diff
}

type keyPair struct {
	name string
	a, b json.RawMessage
}

// keysOf pairs the fields of two JSON objects by name, sorted; a side that is not an object has none.
func keysOf(a, b json.RawMessage) []keyPair {
	var ma, mb map[string]json.RawMessage
	_ = json.Unmarshal(a, &ma)
	_ = json.Unmarshal(b, &mb)
	keys := map[string]bool{}
	for k := range ma {
		keys[k] = true
	}
	for k := range mb {
		keys[k] = true
	}
	out := make([]keyPair, 0, len(keys))
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		out = append(out, keyPair{k, ma[k], mb[k]})
	}
	return out
}

// diffJSON walks objects on both sides and records each differing leaf; lists and values are leaves.
func diffJSON(path string, a, b json.RawMessage, out *[]api.FieldDiff) {
	if bytes.Equal(a, b) {
		return
	}
	isObj := func(v json.RawMessage) bool { return len(v) > 0 && v[0] == '{' }
	if (isObj(a) || a == nil) && (isObj(b) || b == nil) {
		n := len(*out)
		for _, k := range keysOf(a, b) {
			diffJSON(path+"."+k.name, k.a, k.b, out)
		}
		if len(*out) > n {
			return
		}
	}
	*out = append(*out, api.FieldDiff{Path: path, Before: a, After: b})
}

// fleetChanged records a version change.
func (c *Controller) fleetChanged(version int64, by string, changes []api.Change) {
	names := make([]string, 0, len(changes))
	for _, ch := range changes {
		names = append(names, ch.Name)
	}
	if by == "" {
		by = api.PrincipalLocal
	}
	c.logf("fleet v%d by %s: %s", version, by, strings.Join(names, ", "))
	d := api.FleetChangedData{Version: version, By: by, Names: names}
	c.publishControllerEvent("", api.EventFleetChanged, d, names, &d)
}
