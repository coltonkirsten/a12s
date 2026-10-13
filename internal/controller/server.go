package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/computerd"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// maxFleetBody bounds PUT /v1/fleet, which carries preset tars.
const maxFleetBody = 64 << 20

type httpError struct {
	status int
	body   api.Error
}

func (e *httpError) Error() string { return e.body.Message }

func errorf(status int, code api.ErrorCode, format string, args ...any) error {
	return &httpError{status: status, body: api.Error{Code: code, Message: fmt.Sprintf(format, args...)}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code api.ErrorCode, msg string) {
	writeJSON(w, status, api.Error{Code: code, Message: msg})
}

// fail writes err as an api.Error: Controller errors carry their status,
// computerd's 409 and 503 pass through as busy/conflict and asleep.
func fail(w http.ResponseWriter, err error) {
	var he *httpError
	var ce *computerd.Error
	switch {
	case errors.As(err, &he):
		writeJSON(w, he.status, he.body)
	case errors.As(err, &ce):
		switch ce.Status {
		case http.StatusConflict:
			code := api.ErrBusy
			if strings.Contains(ce.Message, "requestId") {
				code = api.ErrConflict
			}
			writeError(w, ce.Status, code, ce.Message)
		case http.StatusServiceUnavailable:
			writeError(w, ce.Status, api.ErrAsleep, ce.Message)
		case http.StatusNotFound:
			writeError(w, ce.Status, api.ErrNotFound, ce.Message)
		case http.StatusBadRequest:
			writeError(w, ce.Status, api.ErrInvalid, ce.Message)
		default:
			writeError(w, http.StatusBadGateway, api.ErrInternal, ce.Error())
		}
	default:
		writeError(w, http.StatusInternalServerError, api.ErrInternal, err.Error())
	}
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "body: "+err.Error())
		return false
	}
	return true
}

// Handler is the owner API, behind the owner bearer token or a scoped token.
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/fleet", allow("", "", c.handleGetFleet))
	mux.HandleFunc("PUT /v1/fleet", allow("", "", c.handlePutFleet))
	mux.HandleFunc("GET /v1/status", allow(api.ActionStatus, "", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, c.Status(r.Context()))
	}))
	mux.HandleFunc("GET /v1/agent-computers", allow(api.ActionStatus, "", c.handleAgentComputers))
	mux.HandleFunc("GET /v1/agent-computers/{c}", allow(api.ActionStatus, "c", c.handleAgentComputer))
	mux.HandleFunc("PUT /v1/agent-computers/{c}", allow("", "", c.handlePutAgentComputer))
	mux.HandleFunc("PATCH /v1/agent-computers/{c}", allow("", "", c.handlePatchAgentComputer))
	mux.HandleFunc("DELETE /v1/agent-computers/{c}", allow("", "", c.handleDeleteAgentComputer))
	mux.HandleFunc("POST /v1/agent-computers/{c}/turns", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusGone, api.Error{Code: api.ErrGone, Message: "turns are started from the inbox now (one way in)",
			Fix: "POST /v1/agent-computers/" + r.PathValue("c") + "/inbox, or a12s prompt / a12s send"})
	})
	mux.HandleFunc("GET /v1/agent-computers/{c}/events", allow(api.ActionEvents, "c", c.handleEvents))
	mux.HandleFunc("POST /v1/agent-computers/{c}/turns/{id}/stop", allow(api.ActionStop, "c", c.handleStop))
	mux.HandleFunc("POST /v1/agent-computers/{c}/power", allow(api.ActionPower, "c", c.handlePower))
	mux.HandleFunc("POST /v1/apply", allow("", "", c.handleApply))
	mux.HandleFunc("POST /v1/agent-computers/{c}/shell", allow("", "", c.handleShell))
	mux.HandleFunc("GET /v1/drives", allow("", "", c.handleDrives))
	mux.HandleFunc("DELETE /v1/drives/{d}", allow("", "", c.handleRemoveDrive))

	mux.HandleFunc("POST /v1/agent-computers/{c}/inbox", allow(api.ActionSend, "c", c.handleEnqueue))
	mux.HandleFunc("GET /v1/agent-computers/{c}/inbox", allow(api.ActionInbox, "c", c.handleInboxList))
	mux.HandleFunc("GET /v1/agent-computers/{c}/inbox/{id}", allowSendOrInbox(c.handleInboxGet))
	mux.HandleFunc("GET /v1/agent-computers/{c}/inbox/{id}/events", allowSendOrInbox(c.handleMessageEvents))
	mux.HandleFunc("DELETE /v1/agent-computers/{c}/inbox/{id}", allowSendOrInbox(c.handleWithdraw))
	mux.HandleFunc("POST /v1/agent-computers/{c}/inbox/clear", allow(api.ActionInbox, "c", c.handleInboxClear))
	mux.HandleFunc("POST /v1/agent-computers/{c}/inbox/hold", allow(api.ActionInbox, "c", c.handleInboxHold))
	mux.HandleFunc("POST /v1/agent-computers/{c}/inbox/resume", allow(api.ActionInbox, "c", c.handleInboxResume))
	mux.HandleFunc("GET /v1/services", allow(api.ActionStatus, "", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, api.ServicesResponse{Services: c.Services()})
	}))
	mux.HandleFunc("GET /v1/services/{s}", allow(api.ActionStatus, "", c.handleService))
	mux.HandleFunc("PUT /v1/services/{s}", allow("", "", c.handlePutService))
	mux.HandleFunc("PATCH /v1/services/{s}", allow("", "", c.handlePatchService))
	mux.HandleFunc("DELETE /v1/services/{s}", allow("", "", c.handleDeleteService))
	mux.HandleFunc("GET /v1/services/{s}/logs", allow("", "", c.handleServiceLogs))
	mux.HandleFunc("POST /v1/services/{s}/power", allow("", "", c.handleServicePower))
	mux.HandleFunc("POST /v1/services/{s}/shell", allow("", "", c.handleServiceShell))
	mux.HandleFunc("POST /v1/services/{s}/login", allow("", "", c.handleLogin))
	mux.HandleFunc("DELETE /v1/services/{s}/login", allow("", "", c.handleLoginEnd))
	mux.HandleFunc("GET /v1/credentials", allow("", "", c.handleCredentials))
	mux.HandleFunc("PUT /v1/credentials/{name}", allow("", "", c.handlePutCredential))
	mux.HandleFunc("DELETE /v1/credentials/{name}", allow("", "", c.handleDeleteCredential))
	mux.HandleFunc("POST /v1/tokens", allow("", "", c.handleTokenCreate))
	mux.HandleFunc("GET /v1/tokens", allow("", "", c.handleTokens))
	mux.HandleFunc("DELETE /v1/tokens/{name}", allow("", "", c.handleTokenDelete))
	return legacyPaths(c.authenticate(mux))
}

// detached runs fn on the Controller's context so the work finishes even if the caller
// disconnects; once the Controller is stopping, nothing new starts.
func detached[T any](c *Controller, r *http.Request, fn func(ctx context.Context) (T, error)) (T, error, bool) {
	type res struct {
		v   T
		err error
	}
	done := make(chan res, 1)
	if !c.spawn(func() {
		v, err := fn(c.ctx)
		done <- res{v, err}
	}) {
		var zero T
		return zero, errorf(http.StatusServiceUnavailable, api.ErrInternal, "the Controller is shutting down"), true
	}
	select {
	case x := <-done:
		return x.v, x.err, true
	case <-r.Context().Done():
		var zero T
		return zero, r.Context().Err(), false
	}
}

func (c *Controller) handleApply(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	res, err, ok := detached(c, r, func(ctx context.Context) (api.ApplyResponse, error) { return c.Apply(ctx, req) })
	if !ok {
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (c *Controller) handlePower(w http.ResponseWriter, r *http.Request) {
	var req api.PowerRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	name := r.PathValue("c")
	_, err, ok := detached(c, r, func(ctx context.Context) (struct{}, error) { return struct{}{}, c.SetPower(ctx, name, req) })
	if !ok {
		return
	}
	if err != nil {
		inboxFail(w, err)
		return
	}
	cs := c.computerStatus(r.Context(), name)
	if req.ClearInbox {
		c.mu.Lock()
		n := c.rtLocked(name).lastClear
		c.mu.Unlock()
		cs.InboxCleared = &n
	}
	writeJSON(w, http.StatusOK, cs)
}

// awake returns the computerd client of a Computer accepting work, or an
// error: 404 unknown, 503 asleep or stopping.
func (c *Controller) awake(name string) (computerdClient, error) {
	c.mu.Lock()
	known := c.state.Fleet != nil
	if known {
		_, known = c.state.Fleet.Computers[name]
	}
	phase := c.rtLocked(name).phase
	c.mu.Unlock()
	if !known {
		return nil, errorf(http.StatusNotFound, api.ErrNotFound, "no computer %q in the Fleet", name)
	}
	if phase != api.PhaseReady && phase != api.PhaseBusy {
		return nil, errorf(http.StatusServiceUnavailable, api.ErrAsleep, "%s is %s", name, phase)
	}
	return c.client(name), nil
}

func (c *Controller) handleStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	if r.URL.Query().Get("hold") == "true" {
		// stop --hold: the hold first, under the admission gate, then the interrupt, so no Batch
		// slips in between.
		if !callerOf(r).may(api.ActionInbox, name) {
			writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden, Message: "stop --hold needs the inbox action", Fix: "use a token with inbox on " + name})
			return
		}
		c.mu.Lock()
		rt := c.rtLocked(name)
		c.mu.Unlock()
		rt.admit.Lock()
		err := c.inbox.Hold(r.Context(), name, api.HoldOperator, "a12s stop --hold", callerOf(r).principal)
		rt.admit.Unlock()
		if err != nil {
			inboxFail(w, err)
			return
		}
	}
	cl, err := c.awake(name)
	if err == nil {
		err = cl.Stop(r.Context(), r.PathValue("id"))
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"turnId": r.PathValue("id"), "status": "stopping"})
}

// handleEvents proxies computerd's SSE stream, flushing every message. The
// client resumes with Last-Event-ID; nothing is buffered here.
func (c *Controller) handleEvents(w http.ResponseWriter, r *http.Request) {
	cl, err := c.awake(r.PathValue("c"))
	if err != nil {
		fail(w, err)
		return
	}
	s, err := cl.Events(r.Context(), r.Header.Get("Last-Event-ID"), r.URL.Query().Get("turn"))
	if err != nil {
		fail(w, err)
		return
	}
	defer s.Close()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	msgs := make(chan computerd.Message)
	go func() {
		defer close(msgs)
		for {
			m, err := s.Next()
			if err != nil {
				return
			}
			select {
			case msgs <- m:
			case <-r.Context().Done():
				return
			}
		}
	}()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			_, err = io.WriteString(w, ": keepalive\n\n")
		case m, ok := <-msgs:
			if !ok {
				return
			}
			var b strings.Builder
			if m.ID != "" {
				b.WriteString("id: " + m.ID + "\n")
			}
			if m.Event != "" {
				b.WriteString("event: " + m.Event + "\n")
			}
			for _, line := range strings.Split(string(m.Data), "\n") {
				b.WriteString("data: " + line + "\n")
			}
			b.WriteString("\n")
			_, err = io.WriteString(w, b.String())
		}
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// handleShell audits `a12s shell` in the Computer's journal and names the
// container for the CLI to exec into.
func (c *Controller) handleShell(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	var req api.ShellRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	cl, err := c.awake(name)
	if err != nil {
		fail(w, err)
		return
	}
	user := "1000:1000"
	if req.Root {
		user = "0:0"
	}
	if err := cl.Diagnostic(r.Context(), "shell opened", map[string]any{"user": user, "command": req.Command}); err != nil {
		fail(w, err)
		return
	}
	c.logf("%s: shell opened as %s", name, user)
	writeJSON(w, http.StatusOK, api.ShellResponse{Container: c.containerName(name), User: user})
}

func (c *Controller) Status(ctx context.Context) (res api.StatusResponse) {
	c.mu.Lock()
	f := c.state.Fleet
	res = api.StatusResponse{Fleet: c.cfg.Fleet, EgressEnforcement: fleet.EnforcementEnforced, AgentComputers: []api.ComputerStatus{}, Computers: []api.ComputerStatus{}}
	defer func() { res.Computers = res.AgentComputers }()
	needCreds := false
	if f != nil {
		for _, comp := range f.Computers {
			needCreds = needCreds || len(comp.Credentials) > 0
		}
	}
	res.CredentialsLoaded = len(c.creds) > 0 || !needCreds
	unenforced := false // in any Computer's active generation
	for _, rec := range c.state.Computers {
		unenforced = unenforced || rec.Active.Unenforced
	}
	c.mu.Unlock()
	if n := c.timeScaleNote(); n != "" {
		res.Warnings = append(res.Warnings, n)
	}
	if f == nil {
		res.Warnings = append(res.Warnings, "no Fleet applied; run a12s up")
		return res
	}
	if unenforced {
		res.EgressEnforcement = fleet.EnforcementUnenforced
		res.Warnings = append(res.Warnings, "egress is NOT enforced (egress.enforcement: unenforced)")
	}
	if !res.CredentialsLoaded {
		res.Warnings = append(res.Warnings, "credentials not loaded (the Controller restarted); run a12s up")
	}

	names := f.ComputerNames()
	res.AgentComputers = make([]api.ComputerStatus, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() { res.AgentComputers[i] = c.computerStatus(ctx, name) })
	}
	wg.Wait()

	if c.inbox != nil {
		if sums, err := c.inbox.Inboxes(ctx); err == nil {
			byName := map[string]api.InboxSummary{}
			for _, s := range sums {
				byName[s.Computer] = s
				if s.Orphaned && s.Pending+s.Claimed > 0 {
					res.OrphanedInboxes = append(res.OrphanedInboxes, s)
					res.Warnings = append(res.Warnings, fmt.Sprintf("orphaned inbox %s: %d pending (a12s inbox clear %s --orphan discards them)", s.Computer, s.Pending, s.Computer))
				}
			}
			for i := range res.AgentComputers {
				if s, ok := byName[res.AgentComputers[i].Name]; ok {
					res.AgentComputers[i].Inbox = &s
					if s.Held {
						res.AgentComputers[i].Warnings = append(res.AgentComputers[i].Warnings, fmt.Sprintf("inbox held (%s); a12s inbox resume %s", s.HoldReason, s.Computer))
					}
					if s.OverLimit {
						res.AgentComputers[i].Warnings = append(res.AgentComputers[i].Warnings, fmt.Sprintf("inbox over its limit (%d/%d); new messages are refused until it drains", s.Usage.Messages, s.Limits.MaxMessages.Value))
					}
				}
			}
		}
	}

	res.Services = c.Services()
	for _, s := range res.Services {
		if s.EgressMode == api.EgressUnenforced {
			res.EgressEnforcement = fleet.EnforcementUnenforced
		}
	}

	if drives, err := c.drives(ctx); err == nil {
		var orphans []string
		for _, d := range drives {
			if d.Orphaned {
				res.OrphanedDrives = append(res.OrphanedDrives, d)
				orphans = append(orphans, d.Name)
			}
		}
		if len(orphans) > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("orphaned drives (no Computer in the Fleet uses them): %s; a12s drive rm <name> destroys one", strings.Join(orphans, ", ")))
		}
	}
	return res
}

func (c *Controller) computerStatus(ctx context.Context, name string) (out api.ComputerStatus) {
	c.mu.Lock()
	comp := c.state.Fleet.Computers[name]
	cs := api.ComputerStatus{
		Name:     name,
		Harness:  api.Harness{Kind: comp.Harness.Kind, Version: comp.Harness.Version, Model: comp.Harness.Model},
		Image:    comp.Image,
		Autonomy: comp.Autonomy,
		HomeSize: comp.Home.Size,
	}
	for _, d := range comp.Drives {
		cs.Drives = append(cs.Drives, d.Name)
	}
	if rec := c.state.Computers[name]; rec != nil {
		cs.Generation, cs.Session, cs.CredentialGeneration = rec.Generation, rec.Session, rec.CredentialGeneration
		cs.ResumePending = rec.ResumePending
		cs.EgressMode = c.egressModeLocked(rec)
		cs.Seccomp = rec.Active.Seccomp
		if rec.pending() {
			cs.StagedGeneration = rec.StagedGeneration
			cs.Warnings = append(cs.Warnings, fmt.Sprintf("generation %d staged; applies when idle (a12s apply --interrupt to force)", rec.StagedGeneration))
		}
	}
	r := c.rtLocked(name)
	cs.Phase, cs.Error, cs.ActiveTurn = r.phase, r.failure, r.activeTurn
	c.mu.Unlock()
	defer func() { out.Power = c.powerStatus(ctx, name, out.Phase) }()

	if cs.EgressMode == api.EgressAllowAll {
		cs.Warnings = append(cs.Warnings, `egress allows any public host ("*")`)
	}
	if cs.Phase == api.PhaseReady || cs.Phase == api.PhaseBusy {
		sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		st, err := c.client(name).Status(sctx)
		cancel()
		if err != nil {
			cs.Warnings = append(cs.Warnings, "computerd did not answer: "+err.Error())
		} else {
			cs.Session, cs.ActiveTurn, cs.ResumePending = st.Session, st.ActiveTurn, st.ResumePending
			cs.Phase = api.PhaseReady
			if st.ActiveTurn != "" || st.Phase == "busy" {
				cs.Phase = api.PhaseBusy
			}
			cs.CredentialGeneration = st.CredentialGeneration
			cs.Harness = st.Harness
			if st.ResumePending {
				cs.Warnings = append(cs.Warnings, "resume pending: the next prompt reopens the session")
			}
		}
	}
	return cs
}

func (c *Controller) handleDrives(w http.ResponseWriter, r *http.Request) {
	drives, err := c.drives(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.DrivesResponse{Drives: drives})
}

func (c *Controller) drives(ctx context.Context) ([]api.DriveRow, error) {
	obs, err := c.drv.Observe(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	f := c.state.Fleet
	c.mu.Unlock()
	if f == nil {
		f = &fleet.Fleet{}
	}
	rows := []api.DriveRow{}
	for id, d := range obs.Drives {
		row := api.DriveRow{Name: id, AttachedTo: d.AttachedTo}
		if svc, ok := driver.ServiceOfStateDrive(id); ok {
			row.Name, row.Kind, row.Service = driver.StateDriveName(svc), api.DriveState, svc
			s, declared := f.Services[svc]
			row.Orphaned = !declared
			if declared {
				c.mu.Lock()
				if c.state.Fleet != nil && c.state.Manifests[svc] != nil {
					row.Size = c.serviceStateSizeLocked(svc)
				}
				c.mu.Unlock()
			}
			_ = s
		} else if owner, ok := strings.CutPrefix(id, "home-"); ok {
			row.Kind, row.Computer = api.DriveHome, owner
			comp, declared := f.Computers[owner]
			row.Size, row.Orphaned = comp.Home.Size, !declared
		} else {
			name := strings.TrimPrefix(id, "drive-")
			row.Name, row.Kind = name, api.DriveWork
			fd, declared := f.Drives[name]
			row.Size, row.Access, row.Orphaned = fd.Size, fd.Access, !declared
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

// handleRemoveDrive is `a12s drive rm`: the only path that destroys data.
func (c *Controller) handleRemoveDrive(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("d")
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	obs, err := c.drv.Observe(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	id := driver.WorkDriveID(name)
	if _, ok := obs.Drives[id]; !ok && strings.HasPrefix(name, "home-") {
		id = name
	}
	if svc, ok := strings.CutSuffix(name, "-state"); ok {
		if _, isWork := obs.Drives[id]; !isWork {
			id = driver.StateDriveID(svc)
		}
	}
	d, ok := obs.Drives[id]
	if !ok {
		fail(w, errorf(http.StatusNotFound, api.ErrNotFound, "no drive %q", name))
		return
	}
	if len(d.AttachedTo) > 0 {
		fail(w, errorf(http.StatusConflict, api.ErrAttached, "drive %s is attached to %s", name, strings.Join(d.AttachedTo, ", ")))
		return
	}
	if err := c.drv.RemoveDrive(r.Context(), id); err != nil {
		if errors.Is(err, driver.ErrDriveAttached) {
			err = errorf(http.StatusConflict, api.ErrAttached, "%v", err)
		}
		fail(w, err)
		return
	}
	c.logf("drive %s destroyed", name)
	w.WriteHeader(http.StatusNoContent)
}

func (c *Controller) handleService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("s")
	if !c.isService(name) {
		fail(w, errorf(http.StatusNotFound, api.ErrNotFound, "no service %q in the Fleet", name))
		return
	}
	writeJSON(w, http.StatusOK, c.serviceStatus(name))
}

func (c *Controller) handleServicePower(w http.ResponseWriter, r *http.Request) {
	var req api.ServicePowerRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	name := r.PathValue("s")
	_, err, ok := detached(c, r, func(ctx context.Context) (struct{}, error) { return struct{}{}, c.SetServicePower(ctx, name, req) })
	if !ok {
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c.serviceStatus(name))
}

// handleServiceLogs streams `a12s service logs`: the Controller's audit
// lines for the Service, then its container output, following both.
func (c *Controller) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("s")
	if !c.isService(name) {
		fail(w, errorf(http.StatusNotFound, api.ErrNotFound, "no service %q in the Fleet", name))
		return
	}
	follow := r.URL.Query().Get("follow") == "true"
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = c.writeServiceLogs(r.Context(), name, follow, tail, func(s string) error {
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		return rc.Flush()
	})
}

func (c *Controller) handleServiceShell(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("s")
	var req api.ShellRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	c.mu.Lock()
	m := c.state.Manifests[name]
	ok := c.state.Fleet != nil && c.state.Fleet.HasService(name)
	c.mu.Unlock()
	if !ok {
		fail(w, errorf(http.StatusNotFound, api.ErrNotFound, "no service %q in the Fleet", name))
		return
	}
	if p := c.servicePhase(name); p != api.PhaseReady && p != api.PhaseUnhealthy && p != api.PhaseStarting {
		fail(w, errorf(http.StatusServiceUnavailable, api.ErrAsleep, "service %s is %s", name, p))
		return
	}
	user := serviceUser(m)
	if req.Root {
		user = "0:0"
	}
	c.auditf(name, "shell opened as %s (%s)", user, strings.Join(req.Command, " "))
	writeJSON(w, http.StatusOK, api.ShellResponse{Container: c.containerName(name), User: user})
}
