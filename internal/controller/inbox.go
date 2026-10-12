package controller

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

type caller struct {
	principal string     // "local" or "token:<name>"
	token     *api.Token // nil for the owner
}

type callerKey struct{}

func callerOf(r *http.Request) caller {
	if c, ok := r.Context().Value(callerKey{}).(caller); ok {
		return c
	}
	return caller{}
}

func (k caller) owner() bool { return k.principal == api.PrincipalLocal && k.token == nil }

func (k caller) may(action, computer string) bool {
	return k.owner() || (k.token != nil && inbox.Allows(k.token, action, computer))
}

// authenticate resolves the bearer token: the owner token is "local", a
// scoped token is "token:<name>". Anything else is 401.
func (c *Controller) authenticate(next http.Handler) http.Handler {
	owner := []byte("Bearer " + c.cfg.APIToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(h), owner) == 1 {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller{principal: api.PrincipalLocal})))
			return
		}
		if secret, ok := strings.CutPrefix(h, "Bearer "); ok && c.inbox != nil {
			if t, err := c.inbox.LookupToken(r.Context(), secret); err == nil && t != nil {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller{principal: "token:" + t.Name, token: t})))
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, api.Error{Code: api.ErrUnauthorized, Message: "missing or wrong bearer token",
			Fix: "use the owner token in ~/.a12s/<fleet>/controller.json (the CLI does), or a scoped token from a12s token create"})
	})
}

// allow wraps a handler with an action check; computerParam names the path
// value holding the Computer ("" for Fleet-wide actions). An empty action
// means owner only.
func allow(action, computerParam string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k := callerOf(r)
		computer := ""
		if computerParam != "" {
			computer = r.PathValue(computerParam)
		}
		if action == "" && !k.owner() || action != "" && !k.may(action, computer) {
			what := "make this call: it is owner-only"
			if action != "" {
				what = action
				if computer != "" {
					what += " on " + computer
				}
			}
			writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden, Message: k.principal + " may not " + what,
				Fix: "use the owner token, or a token with that action (a12s token create)"})
			return
		}
		h(w, r)
	}
}

func inboxFail(w http.ResponseWriter, err error) {
	var ie *inbox.Error
	switch {
	case errors.As(err, &ie):
		writeJSON(w, ie.Status, api.Error{Code: ie.Code, Message: ie.Message, Fix: ie.Fix, Details: ie.Details})
	case errors.Is(err, inbox.ErrUnknownInbox):
		writeJSON(w, http.StatusNotFound, api.Error{Code: api.ErrNotFound, Message: "no such Computer in the Fleet", Fix: "a12s status lists the Computers"})
	default:
		fail(w, err)
	}
}

func (c *Controller) syncInboxes(ctx context.Context, f *fleet.Fleet, by string) error {
	if c.inbox == nil || f == nil {
		return nil
	}
	declared := map[string]inbox.Config{}
	for _, name := range f.ComputerNames() {
		s := f.ResolveInbox(name)
		declared[name] = inbox.Config{
			Limits: api.InboxLimits{
				MaxMessages: api.LimitValue{Value: s.MaxMessages, Source: s.MaxMessagesSource},
				MaxBytes:    api.LimitValue{Value: s.MaxBytes, Source: s.MaxBytesSource},
			},
			Intro: s.Intro,
		}
	}
	return c.inbox.SyncFleet(ctx, declared, by)
}

func (c *Controller) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	var req api.EnqueueRequest
	if !decode(w, r, inbox.MaxBody*7+64<<10, &req) {
		return
	}
	k := callerOf(r)
	if req.Expires != nil && *req.Expires == 0 {
		c.nowOrNeverEnqueue(w, r, name, k, req)
		return
	}
	res, err := c.inbox.Enqueue(r.Context(), inbox.Enqueue{Computer: name, Principal: k.principal, Req: req}, nil)
	if err != nil {
		c.logf("%s: enqueue by %s refused: %v", name, k.principal, err)
		inboxFail(w, err)
		return
	}
	m := res.Message
	out := api.EnqueueResponse{ID: m.ID, Seq: m.Seq, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt, State: m.State, Position: res.Position, Phase: c.phase(name), Replaced: res.Replaced, Batch: m.Batch, Turn: m.Turn}
	status := http.StatusCreated
	if !res.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// ownOnly returns the via filter for get and withdraw: a caller without the
// inbox action sees only its own Messages.
func ownOnly(k caller, computer string) string {
	if k.may(api.ActionInbox, computer) {
		return ""
	}
	return k.principal
}

func allowSendOrInbox(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k := callerOf(r)
		name := r.PathValue("c")
		if !k.may(api.ActionSend, name) && !k.may(api.ActionInbox, name) {
			writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden, Message: k.principal + " may not read " + name + "'s inbox", Fix: "use a token with send or inbox on " + name})
			return
		}
		h(w, r)
	}
}

func (c *Controller) handleInboxGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	m, err := c.inbox.Get(r.Context(), name, r.PathValue("id"), ownOnly(callerOf(r), name), true)
	if err != nil {
		inboxFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (c *Controller) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("c")
	k := callerOf(r)
	m, err := c.inbox.Withdraw(r.Context(), name, r.PathValue("id"), ownOnly(k, name), k.principal)
	if err != nil {
		inboxFail(w, err)
		return
	}
	c.logf("%s: %s withdrew %s", name, k.principal, m.ID)
	writeJSON(w, http.StatusOK, m)
}

func (c *Controller) handleInboxList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	o := inbox.ListOptions{State: q.Get("state"), Bodies: q.Get("bodies") == "true"}
	if a := q.Get("after"); a != "" {
		n, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, api.ErrInvalid, "after must be a seq")
			return
		}
		o.After = n
	}
	switch o.State {
	case "", "all", api.MsgPending, api.MsgClaimed, api.MsgDelivered, api.MsgDiscarded, api.MsgWithdrawn, api.MsgExpired, api.MsgReplaced:
	default:
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "unknown state "+o.State)
		return
	}
	res, err := c.inbox.List(r.Context(), r.PathValue("c"), o)
	if err != nil {
		inboxFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (c *Controller) handleInboxClear(w http.ResponseWriter, r *http.Request) {
	var req api.ClearRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	name := r.PathValue("c")
	k := callerOf(r)
	n, err := c.inbox.Clear(r.Context(), name, req.ThroughSeq, req.Orphan, "inbox-clear", k.principal)
	if err != nil {
		inboxFail(w, err)
		return
	}
	c.logf("%s: %s cleared %d pending messages", name, k.principal, n)
	writeJSON(w, http.StatusOK, api.ClearResponse{Discarded: n})
}

func (c *Controller) handleInboxHold(w http.ResponseWriter, r *http.Request) {
	var req api.HoldRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	name := r.PathValue("c")
	// Under the admission gate, so a Batch claimed before the hold can't
	// POST after it (postBatch re-checks the hold inside the same gate).
	c.mu.Lock()
	rt := c.rtLocked(name)
	c.mu.Unlock()
	rt.admit.Lock()
	err := c.inbox.Hold(r.Context(), name, api.HoldOperator, req.Reason, callerOf(r).principal)
	rt.admit.Unlock()
	if err != nil {
		inboxFail(w, err)
		return
	}
	c.writeSummary(w, r, name)
}

func (c *Controller) handleInboxResume(w http.ResponseWriter, r *http.Request) {
	var req api.ResumeRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	switch req.Session {
	case "", api.SessionCurrent, api.SessionNew:
	default:
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "session must be current or new")
		return
	}
	name := r.PathValue("c")
	if err := c.inbox.Resume(r.Context(), name, req.Session == api.SessionNew, callerOf(r).principal); err != nil {
		inboxFail(w, err)
		return
	}
	c.writeSummary(w, r, name)
}

func (c *Controller) writeSummary(w http.ResponseWriter, r *http.Request, name string) {
	sum, err := c.inbox.Summary(r.Context(), name)
	if err != nil {
		inboxFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// tokenNameRE limits token names to a DNS label so "token:<name>" never needs escaping in a frame.
var tokenNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func (c *Controller) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var req api.TokenRequest
	if !decode(w, r, 1<<20, &req) {
		return
	}
	if !tokenNameRE.MatchString(req.Name) {
		writeJSON(w, http.StatusBadRequest, api.Error{Code: api.ErrInvalid, Message: "token name " + strconv.Quote(req.Name) + " is not a lowercase DNS label", Fix: "use a-z, 0-9 and -, at most 63 characters"})
		return
	}
	if len(req.Actions) == 0 {
		writeJSON(w, http.StatusBadRequest, api.Error{Code: api.ErrInvalid, Message: "a token needs at least one action", Fix: "pass --actions " + strings.Join(api.TokenActions, ",")})
		return
	}
	for _, a := range req.Actions {
		if !slices.Contains(api.TokenActions, a) {
			writeJSON(w, http.StatusBadRequest, api.Error{Code: api.ErrInvalid, Message: "unknown action " + strconv.Quote(a), Fix: "actions are " + strings.Join(api.TokenActions, ", ")})
			return
		}
	}
	if len(req.Computers) == 0 {
		writeJSON(w, http.StatusBadRequest, api.Error{Code: api.ErrInvalid, Message: "a token needs at least one Computer", Fix: "pass --computers name,... or --computers '*'"})
		return
	}
	for _, n := range req.Computers {
		if n != "*" && !fleet.ValidName(n) {
			writeError(w, http.StatusBadRequest, api.ErrInvalid, "invalid Computer name "+strconv.Quote(n))
			return
		}
	}
	t, err := c.inbox.CreateToken(r.Context(), req.Name, req.Actions, req.Computers)
	if err != nil {
		inboxFail(w, err)
		return
	}
	c.logf("token %s created (actions %s, computers %s)", t.Name, strings.Join(t.Actions, ","), strings.Join(t.Computers, ","))
	writeJSON(w, http.StatusCreated, t)
}

func (c *Controller) handleTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := c.inbox.Tokens(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.TokensResponse{Tokens: ts})
}

func (c *Controller) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	if err := c.inbox.DeleteToken(r.Context(), r.PathValue("name")); err != nil {
		inboxFail(w, err)
		return
	}
	c.logf("token %s revoked", r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

// expiryLoop arms a timer on the earliest expiresAt, re-armed on every inbox change, and expires
// everything overdue on start. It also prunes, hourly, what is past retention and sealed.
func (c *Controller) expiryLoop(ctx context.Context) {
	var pruned time.Time
	for ctx.Err() == nil {
		if time.Since(pruned) > time.Hour {
			pruned = time.Now()
			if n, err := c.inbox.Prune(ctx); err != nil {
				c.logf("inbox prune: %v", err)
			} else if n > 0 {
				c.logf("inbox: pruned %d messages past retention", n)
			}
		}
		if _, err := c.inbox.ExpireDue(ctx); err != nil && ctx.Err() == nil {
			c.logf("inbox expiry: %v", err)
		}
		wait := time.Hour
		if next, ok, err := c.inbox.NextExpiry(ctx); err == nil && ok {
			wait = max(time.Until(next), 0) + time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-c.inboxKick:
			t.Stop()
		}
	}
}

func (c *Controller) inboxChanged(name string) {
	select {
	case c.inboxKick <- struct{}{}:
	default:
	}
	c.inboxBroadcast()
	c.kick(name)
}
