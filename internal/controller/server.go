package controller

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
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

// httpError is an API error with its HTTP status.
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

// Handler is the CLI API (PLAN "Controller API"), behind the bearer token.
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/fleet", c.handlePutFleet)
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, c.Status(r.Context()))
	})
	mux.HandleFunc("POST /v1/computers/{c}/turns", c.handleTurn)
	mux.HandleFunc("GET /v1/computers/{c}/events", c.handleEvents)
	mux.HandleFunc("POST /v1/computers/{c}/turns/{id}/stop", c.handleStop)
	mux.HandleFunc("POST /v1/computers/{c}/power", c.handlePower)
	mux.HandleFunc("POST /v1/apply", c.handleApply)
	mux.HandleFunc("POST /v1/computers/{c}/shell", c.handleShell)
	mux.HandleFunc("GET /v1/drives", c.handleDrives)
	mux.HandleFunc("DELETE /v1/drives/{d}", c.handleRemoveDrive)
	token := []byte("Bearer " + c.cfg.APIToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), token) != 1 {
			writeError(w, http.StatusUnauthorized, api.ErrUnauthorized, "missing or wrong bearer token")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// detached runs fn on the Controller's context so the work finishes even if
// the caller disconnects, and waits for it while the caller is there. The
// work is a background goroutine Wait waits for (review #58); once the
// Controller is stopping, nothing new starts.
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

func (c *Controller) handlePutFleet(w http.ResponseWriter, r *http.Request) {
	var req api.PutFleetRequest
	if !decode(w, r, maxFleetBody, &req) {
		return
	}
	res, err, ok := detached(c, r, func(ctx context.Context) (api.PutFleetResponse, error) { return c.PutFleet(ctx, req) })
	if !ok {
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c.computerStatus(r.Context(), name))
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

func (c *Controller) handleTurn(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	var req api.TurnRequest
	if !decode(w, r, 4<<20, &req) {
		return
	}
	// Admission holds the Computer's gate until computerd has accepted the
	// turn, so a sleep or restage that closes it next sees the turn.
	// Busy comes from computerd's events and status: an idempotent retry
	// of a finished request starts nothing.
	c.mu.Lock()
	rt := c.rtLocked(name)
	c.mu.Unlock()
	rt.admit.Lock()
	defer rt.admit.Unlock()
	cl, err := c.awake(name)
	if err == nil {
		c.mu.Lock()
		if rt.closed {
			err = errorf(http.StatusServiceUnavailable, api.ErrAsleep, "%s is stopping or being updated", name)
		}
		c.mu.Unlock()
	}
	if err != nil {
		fail(w, err)
		return
	}
	res, err := cl.StartTurn(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (c *Controller) handleStop(w http.ResponseWriter, r *http.Request) {
	cl, err := c.awake(r.PathValue("c"))
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

// Status is GET /v1/status.
func (c *Controller) Status(ctx context.Context) api.StatusResponse {
	c.mu.Lock()
	f := c.state.Fleet
	res := api.StatusResponse{Fleet: c.cfg.Fleet, EgressEnforcement: fleet.EnforcementEnforced, Computers: []api.ComputerStatus{}}
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
	res.Computers = make([]api.ComputerStatus, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() { res.Computers[i] = c.computerStatus(ctx, name) })
	}
	wg.Wait()

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

func (c *Controller) computerStatus(ctx context.Context, name string) api.ComputerStatus {
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
		if rec.pending() {
			cs.StagedGeneration = rec.StagedGeneration
			cs.Warnings = append(cs.Warnings, fmt.Sprintf("generation %d staged; applies when idle (a12s apply --interrupt to force)", rec.StagedGeneration))
		}
	}
	r := c.rtLocked(name)
	cs.Phase, cs.Error, cs.ActiveTurn = r.phase, r.failure, r.activeTurn
	c.mu.Unlock()

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

// drives lists every drive of the Fleet as observed, with sizes and access
// from the Fleet and orphans marked.
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
		if owner, ok := strings.CutPrefix(id, "home-"); ok {
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
