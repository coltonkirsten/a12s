package controller

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/computerd"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

// postTimeout bounds a Turn POST to computerd; an answer that takes longer
// is "outcome unknown" and settled from the journal (§4.6).
const postTimeout = 10 * time.Second

// drainState coalesces drain requests for one Computer: one drainer runs,
// and a kick while it runs makes it go around once more.
type drainState struct {
	running, again bool
}

// kick asks for a drain of a Computer's inbox (§4.5 "when the drainer
// runs"): on every enqueue, eligibility transition and Controller start.
func (c *Controller) kick(name string) {
	c.mu.Lock()
	if c.drains == nil {
		c.drains = map[string]*drainState{}
	}
	d := c.drains[name]
	if d == nil {
		d = &drainState{}
		c.drains[name] = d
	}
	if d.running {
		d.again = true
		c.mu.Unlock()
		return
	}
	d.running = true
	c.mu.Unlock()
	if !c.spawn(func() {
		for {
			retry := c.drainOnce(name)
			c.mu.Lock()
			if !d.again {
				d.running = false
				c.mu.Unlock()
				if retry {
					c.kickLater(name)
				}
				return
			}
			d.again = false
			c.mu.Unlock()
		}
	}) {
		c.mu.Lock()
		d.running = false
		c.mu.Unlock()
	}
}

// kickLater retries a drain that could not finish (computerd unreachable,
// an answer lost) after a pause.
func (c *Controller) kickLater(name string) {
	c.spawn(func() {
		select {
		case <-c.ctx.Done():
		case <-time.After(c.timing.Poll * 20):
			c.kick(name)
		}
	})
}

// kickAll drains every Computer of the Fleet (Controller start).
func (c *Controller) kickAll() {
	c.mu.Lock()
	var names []string
	if c.state.Fleet != nil {
		names = c.state.Fleet.ComputerNames()
	}
	c.mu.Unlock()
	for _, n := range names {
		c.kick(n)
	}
}

// drainOnce is one idle boundary (§4.5): settle any claimed Batch, leave a
// staged generation to applyStaged, run an owed continuation, then claim
// and POST the next Batch. It reports whether it should be retried later.
func (c *Controller) drainOnce(name string) (retry bool) {
	if c.inbox == nil {
		return false
	}
	ctx := c.ctx
	c.mu.Lock()
	known := c.state.Fleet != nil
	if known {
		_, known = c.state.Fleet.Computers[name]
	}
	c.mu.Unlock()
	if !known {
		return false
	}
	unlock := c.lockComputer(name)
	defer unlock()

	// (0) Settle before anything else: a restart never delivers newer
	// Messages ahead of older claimed ones.
	settled, retry := c.settleOpen(ctx, name)
	if !settled {
		return retry
	}
	reason, st := c.deliverable(ctx, name)
	switch {
	case reason == "continuation":
		// (2) Work already in progress runs before the inbox.
		c.runContinuation(ctx, name, st.Continuation)
		return false
	case reason == "unreachable":
		return true
	case reason != "":
		return false
	}
	// (3) Drain.
	b, err := c.inbox.Claim(ctx, name)
	if err != nil {
		c.logf("%s: claim: %v", name, err)
		return true
	}
	if b == nil {
		return false
	}
	c.logf("%s: claimed %s (%d messages)", name, b.ID, len(b.Request.Inbox.Messages))
	outcome := c.postBatch(ctx, name, b)
	return outcome == postUnknown
}

// deliverable says why a Computer can't take a Batch now ("" if it can):
// busy, asleep, not_ready, held, continuation (owed), unreachable. It needs
// the Computer's lock.
func (c *Controller) deliverable(ctx context.Context, name string) (string, api.ComputerdStatus) {
	c.mu.Lock()
	rt := c.rtLocked(name)
	phase, closed, transition := rt.phase, rt.closed, rt.transition
	staged := false
	if rec := c.state.Computers[name]; rec != nil {
		staged = rec.pending()
	}
	c.mu.Unlock()
	switch {
	case phase == api.PhaseAsleep || phase == api.PhaseStopping:
		return "asleep", api.ComputerdStatus{}
	case phase == api.PhaseBusy:
		return "busy", api.ComputerdStatus{}
	case phase != api.PhaseReady || closed || transition || staged:
		// (1) A staged generation applies before the next drain (applyStaged kicks).
		return "not_ready", api.ComputerdStatus{}
	}
	if held, _, err := c.inbox.Held(ctx, name); err == nil && held {
		return "held", api.ComputerdStatus{}
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	st, err := c.client(name).Status(sctx)
	cancel()
	switch {
	case err != nil:
		return "unreachable", st
	case st.ActiveTurn != "" || st.Phase == "busy":
		return "busy", st
	case st.Phase != "ready":
		return "not_ready", st
	case st.Continuation != "":
		return "continuation", st
	}
	return "", st
}

type postOutcome int

const (
	postAccepted postOutcome = iota
	postReturned             // computerd refused before acceptance; the Batch went back
	postUnknown              // no answer: settle from the journal later
)

// postBatch sends a claimed Batch to computerd under the admission gate,
// so a sleep or restage that closes it next sees the Turn (§4.5).
func (c *Controller) postBatch(ctx context.Context, name string, b *inbox.Batch) postOutcome {
	c.mu.Lock()
	rt := c.rtLocked(name)
	c.mu.Unlock()
	rt.admit.Lock()
	defer rt.admit.Unlock()
	c.mu.Lock()
	closed := rt.closed
	c.mu.Unlock()
	if closed {
		c.returnBatch(ctx, name, b, inbox.ReturnOptions{Reason: "not_ready"})
		return postReturned
	}
	return c.sendBatch(ctx, name, b, b.Request)
}

// sendBatch POSTs a Batch's stored request (or its lookup-only replay) and
// records what computerd said.
func (c *Controller) sendBatch(ctx context.Context, name string, b *inbox.Batch, req api.TurnRequest) postOutcome {
	pctx, cancel := context.WithTimeout(ctx, postTimeout)
	res, err := c.client(name).StartTurn(pctx, req)
	cancel()
	var ce *computerd.Error
	switch {
	case err == nil:
		if err := c.inbox.Accept(ctx, b.ID, res.TurnID); err != nil {
			c.logf("%s: accept %s: %v", name, b.ID, err)
		}
		b.Turn, b.State = res.TurnID, inbox.BatchAccepted
		c.logf("%s: %s accepted as turn %s", name, b.ID, res.TurnID)
		// Events may have raced the answer: settle once now.
		c.settleBatch(ctx, name, b)
		return postAccepted
	case errors.As(err, &ce) && ce.Status == http.StatusConflict && !strings.Contains(ce.Message, "requestId"):
		// Something started a Turn between the status read and the POST.
		c.returnBatch(ctx, name, b, inbox.ReturnOptions{Reason: "busy"})
		return postReturned
	case errors.As(err, &ce) && (ce.Status == http.StatusServiceUnavailable || ce.Status == http.StatusNotFound):
		// Not accepting turns, or a lookup-only replay found nothing (and
		// tombstoned the requestId).
		reason := "not_ready"
		if ce.Status == http.StatusNotFound {
			reason = "refused"
		}
		c.returnBatch(ctx, name, b, inbox.ReturnOptions{Reason: reason})
		return postReturned
	}
	c.logf("%s: POST %s: %v (settling from the journal later)", name, b.ID, err)
	return postUnknown
}

func (c *Controller) returnBatch(ctx context.Context, name string, b *inbox.Batch, o inbox.ReturnOptions) {
	res, err := c.inbox.Return(ctx, b.ID, o)
	if err != nil {
		c.logf("%s: return %s: %v", name, b.ID, err)
		return
	}
	c.logf("%s: %s returned (%s): %d pending, %d expired, %d replaced", name, b.ID, o.Reason, len(res.Pending), len(res.Expired), len(res.Replaced))
	if res.Held != "" {
		c.logf("%s: inbox held: %s", name, res.Held)
	}
}

// settleOpen settles every open Batch of a Computer by the §4.6 table. It
// reports whether none is left open, and whether to retry later.
func (c *Controller) settleOpen(ctx context.Context, name string) (bool, bool) {
	open, err := c.inbox.OpenBatches(ctx, name)
	if err != nil {
		c.logf("%s: open batches: %v", name, err)
		return false, true
	}
	for _, b := range open {
		done, retry := c.settleBatch(ctx, name, b)
		if !done {
			return false, retry
		}
	}
	return true, false
}

// oversizedRE recognizes a harness refusing a prompt for its size; it only
// chooses the hold reason, never whether the Batch was delivered (§4.6).
var oversizedRE = regexp.MustCompile(`(?i)prompt is too long|too many tokens|context (window|length)|maximum context|input is too long|too large`)

// settleBatch settles one Batch from computerd's journal: dispatched is
// delivered whatever happened next; finished without dispatch is returned;
// otherwise it waits. A Batch whose POST outcome is unknown is replayed
// first (lookup-only once its fence passed).
func (c *Controller) settleBatch(ctx context.Context, name string, b *inbox.Batch) (done, retry bool) {
	cl := c.client(name)
	if b.Turn == "" {
		req := b.Request
		if b.FencePassed(time.Now()) {
			req.LookupOnly = true
		}
		switch c.sendBatchReplay(ctx, name, b, req) {
		case postReturned:
			return true, false
		case postUnknown:
			return false, true
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	sum, err := cl.TurnSummary(sctx, b.Turn)
	cancel()
	if err != nil {
		return false, true
	}
	switch {
	case sum.Dispatched:
		hold, diag := "", ""
		switch {
		case sum.ResumeFailed:
			hold, diag = api.HoldResumeFailed, "the harness received the Batch, then its session failed to resume: a12s inbox resume "+name+" (--new for a fresh session)"
		case sum.Finished == api.TurnFailed && oversizedRE.MatchString(sum.Error):
			hold, diag = api.HoldOversized, "turn "+b.Turn+" failed on the Batch's size ("+itoaBytes(b)+"): "+sum.Error
		}
		if err := c.inbox.Deliver(ctx, b.ID, b.Turn, hold, diag); err != nil {
			c.logf("%s: deliver %s: %v", name, b.ID, err)
			return false, true
		}
		c.logf("%s: %s delivered in turn %s", name, b.ID, b.Turn)
		return true, false
	case sum.Finished != "":
		o := inbox.ReturnOptions{CountAttempt: true, Reason: sum.Finished}
		if sum.ResumeFailed {
			o.Hold, o.Diagnostic = api.HoldResumeFailed, "the session could not be resumed, so the Batch was returned: repair it and a12s inbox resume "+name+", or a12s inbox resume "+name+" --new"
		}
		c.returnBatch(ctx, name, b, o)
		return true, false
	}
	return false, false // accepted, not yet dispatched: the next event settles it
}

func itoaBytes(b *inbox.Batch) string {
	return strconv.Itoa(len(b.Request.Prompt)) + " bytes"
}

// sendBatchReplay replays a Batch's POST whose answer was lost.
func (c *Controller) sendBatchReplay(ctx context.Context, name string, b *inbox.Batch, req api.TurnRequest) postOutcome {
	pctx, cancel := context.WithTimeout(ctx, postTimeout)
	res, err := c.client(name).StartTurn(pctx, req)
	cancel()
	var ce *computerd.Error
	switch {
	case err == nil:
		if err := c.inbox.Accept(ctx, b.ID, res.TurnID); err != nil {
			return postUnknown
		}
		b.Turn = res.TurnID
		return postAccepted
	case errors.As(err, &ce) && (ce.Status == http.StatusNotFound || ce.Status == http.StatusConflict && !strings.Contains(ce.Message, "requestId")):
		c.returnBatch(ctx, name, b, inbox.ReturnOptions{Reason: "refused"})
		return postReturned
	}
	return postUnknown
}

// runContinuation starts computerd's owed restart_harness continuation with
// its own id as requestId: a replay gets the same Turn back (§4.5).
func (c *Controller) runContinuation(ctx context.Context, name, id string) {
	c.mu.Lock()
	rt := c.rtLocked(name)
	c.mu.Unlock()
	rt.admit.Lock()
	defer rt.admit.Unlock()
	pctx, cancel := context.WithTimeout(ctx, postTimeout)
	res, err := c.client(name).StartTurn(pctx, api.TurnRequest{RequestID: id, Session: api.SessionCurrent, Continuation: true})
	cancel()
	if err != nil {
		c.logf("%s: continuation %s: %v", name, id, err)
		return
	}
	c.logf("%s: continuation %s started as turn %s", name, id, res.TurnID)
}

// nowOrNeverEnqueue is POST …/inbox with expires: 0 (§4.11): admitted only
// if the Computer takes it at once; the call answers after computerd does.
func (c *Controller) nowOrNeverEnqueue(w http.ResponseWriter, r *http.Request, name string, k caller, req api.EnqueueRequest) {
	ctx := r.Context()
	unlock := c.lockComputer(name)
	reason := ""
	if settled, _ := c.settleOpen(ctx, name); !settled {
		reason = "busy"
	}
	if reason == "" {
		reason, _ = c.deliverable(ctx, name)
		switch reason {
		case "continuation", "unreachable":
			reason = "not_ready"
		}
	}
	var batch *inbox.Batch
	res, err := c.inbox.Enqueue(ctx, inbox.Enqueue{Computer: name, Principal: k.principal, Req: req}, func(tx *inbox.Tx, m api.Message) *inbox.Error {
		if reason != "" {
			return inbox.NotIdle(name, reason)
		}
		b, e := tx.ClaimNow(m)
		if e != nil {
			return e
		}
		batch = b
		return nil
	})
	if err != nil {
		unlock()
		c.logf("%s: now-or-never from %s refused: %v", name, k.principal, err)
		inboxFail(w, err)
		return
	}
	if !res.Created || batch == nil {
		// An idempotent repeat: report the row as it stands.
		unlock()
		m := res.Message
		writeJSON(w, http.StatusOK, api.EnqueueResponse{ID: m.ID, Seq: m.Seq, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt, State: m.State, Batch: m.Batch, Turn: m.Turn, Phase: c.phase(name)})
		return
	}
	outcome := c.postBatch(c.ctx, name, batch)
	unlock()
	m := res.Message
	out := api.EnqueueResponse{ID: m.ID, Seq: m.Seq, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt, State: api.MsgClaimed, Batch: batch.ID, Turn: batch.Turn, Phase: c.phase(name)}
	switch outcome {
	case postAccepted:
		if cur, err := c.inbox.Get(ctx, name, m.ID, "", false); err == nil {
			out.State = cur.State
		}
		writeJSON(w, http.StatusCreated, out)
	case postReturned:
		writeJSON(w, http.StatusConflict, api.Error{Code: api.ErrNotIdle, Message: name + " started another turn first; the message expired unsent",
			Fix: "retry later, or omit expires to queue it", Details: map[string]string{"reason": "busy", "id": m.ID}})
	default:
		out.Outcome = "unknown"
		writeJSON(w, http.StatusAccepted, out)
	}
	c.kick(name)
}
