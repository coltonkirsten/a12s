package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

type peerKey struct{}

func peerOf(r *http.Request) (Principal, bool) {
	p, ok := r.Context().Value(peerKey{}).(Principal)
	return p, ok
}

// MTLSHandler: identity counts only while the certificate's serial is the live one in the
// registry. Senders may enqueue only to granted Computers; leases stay Computer-only.
func (c *Controller) MTLSHandler(leases http.Handler) http.Handler {
	mux := http.NewServeMux()
	computerOnly := func(h http.Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if p, _ := peerOf(r); p.Kind != KindComputer {
				writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden, Message: p.String() + " may not use leases", Fix: "leases are for agent computers"})
				return
			}
			h.ServeHTTP(w, r)
		}
	}
	mux.HandleFunc("POST /v1/leases", computerOnly(leases))
	mux.HandleFunc("DELETE /v1/leases", computerOnly(leases))
	mux.HandleFunc("DELETE /v1/leases/{id}", computerOnly(leases))
	mux.HandleFunc("POST /v1/agent-computers/{c}/inbox", c.granted(c.handleEnqueue))
	mux.HandleFunc("GET /v1/agent-computers/{c}/inbox/{id}", c.ownMessages(c.handleInboxGet))
	mux.HandleFunc("DELETE /v1/agent-computers/{c}/inbox/{id}", c.ownMessages(c.handleWithdraw))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p, _ := peerOf(r)
		writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden,
			Message: fmt.Sprintf("%s may not call %s %s on the mTLS listener", p, r.Method, r.URL.Path),
			Fix:     "the mTLS listener serves inbox sends (with a grant) and leases only; owner calls go to the owner API"})
	})
	return legacyPaths(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, entry, err := c.livePeer(r)
		if err != nil {
			writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden, Message: err.Error(), Fix: "a certificate works only while its container runs; a12s injects a fresh one at every start"})
			return
		}
		// The send grant, as a scoped token: the existing inbox handlers
		// then allow send and own-Message reads, and nothing else.
		k := caller{principal: p.String(), token: &api.Token{Name: p.String(), Actions: []string{api.ActionSend}, Computers: entry.Send}}
		ctx := context.WithValue(context.WithValue(r.Context(), peerKey{}, p), callerKey{}, k)
		mux.ServeHTTP(w, r.WithContext(ctx))
	}))
}

func (c *Controller) livePeer(r *http.Request) (Principal, inbox.CertEntry, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return Principal{}, inbox.CertEntry{}, fmt.Errorf("no client certificate")
	}
	leaf := r.TLS.PeerCertificates[0]
	p, err := c.ca.PrincipalOf(leaf)
	if err != nil {
		return p, inbox.CertEntry{}, err
	}
	if c.inbox == nil {
		return p, inbox.CertEntry{}, fmt.Errorf("no certificate registry")
	}
	c.mu.Lock()
	revoked := c.revoked[p.String()]
	live := c.principalRunningLocked(p)
	c.mu.Unlock()
	if revoked || !live {
		return p, inbox.CertEntry{}, fmt.Errorf("the certificate of %s is not active (its container is not running)", p)
	}
	e, ok, err := c.inbox.LiveCert(r.Context(), p.String())
	if err != nil {
		return p, e, err
	}
	if !ok || e.Serial != leaf.SerialNumber.Text(16) || e.Generation != p.Generation {
		c.logf("mTLS: refused %s generation %d serial %s: not the live certificate", p, p.Generation, leaf.SerialNumber.Text(16))
		return p, e, fmt.Errorf("the certificate of %s is not active (its container was stopped, restarted or replaced)", p)
	}
	return p, e, nil
}

// granted admits an enqueue only to a Computer the caller's active
// generation grants, and records a denial where its owner looks: the
// Service's audit log, or the Computer's journal.
func (c *Controller) granted(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := peerOf(r)
		to := r.PathValue("c")
		k := callerOf(r)
		if k.token != nil && slices.Contains(k.token.Computers, to) {
			h(w, r)
			return
		}
		field := "computers." + p.Name + ".grants.send"
		if p.Kind == KindService {
			field = "services." + p.Name + ".grants.send"
		}
		msg := fmt.Sprintf("%s may not send to %s", p.Name, to)
		fix := "add " + to + " to " + field
		c.recordDenial(p, fmt.Sprintf("denied: send to %s (no grant)", to), msg, fix)
		writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrNoGrant, Message: msg, Fix: fix})
	}
}

// ownMessages admits reads and withdrawals; the handlers filter to the
// caller's own Messages (via = principal).
func (c *Controller) ownMessages(h http.HandlerFunc) http.HandlerFunc { return h }

func (c *Controller) recordDenial(p Principal, audit, msg, fix string) {
	if p.Kind == KindService {
		c.auditf(p.Name, "%s", audit)
		return
	}
	c.logf("%s: %s", p.Name, audit)
	c.spawn(func() {
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		defer cancel()
		_ = c.client(p.Name).Diagnostic(ctx, msg, map[string]any{"fix": fix, "source": "controller"})
	})
}

// principalRunningLocked reports whether the Controller believes the
// principal's container is up: a certificate never outlives its container,
// even when the registry could not be updated (fail closed).
func (c *Controller) principalRunningLocked(p Principal) bool {
	switch p.Kind {
	case KindService:
		r := c.srt[p.Name]
		return r != nil && (r.phase == api.PhaseStarting || r.phase == api.PhaseReady || r.phase == api.PhaseUnhealthy)
	case KindComputer:
		r := c.rt[p.Name]
		return r != nil && (r.phase == api.PhaseStarting || r.phase == api.PhaseReady || r.phase == api.PhaseBusy)
	}
	return false
}

func (c *Controller) registerCert(ctx context.Context, p Principal, serial string, send []string) error {
	if c.inbox == nil {
		return nil
	}
	if err := c.inbox.RegisterCert(ctx, inbox.CertEntry{Principal: p.String(), Generation: p.Generation, Serial: serial, Send: send}); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.revoked, p.String())
	c.mu.Unlock()
	return nil
}

// unregisterCert deletes a principal's live certificate before its
// container is stopped, removed or restarted. It is refused in memory
// first, so a failed registry write still fails closed.
func (c *Controller) unregisterCert(kind, name string) {
	c.mu.Lock()
	c.revoked[kind+":"+name] = true
	c.mu.Unlock()
	if c.inbox == nil {
		return
	}
	if err := c.inbox.UnregisterCert(context.WithoutCancel(c.ctx), kind+":"+name); err != nil {
		c.logf("%s:%s: unregister certificate: %v (refused in memory until its next start)", kind, name, err)
	}
}

// reconcileCerts drops registry entries whose container is not running
// (it died, or was stopped, while the Controller was down).
func (c *Controller) reconcileCerts(ctx context.Context, running map[string]bool) {
	if c.inbox == nil {
		return
	}
	entries, err := c.inbox.LiveCerts(ctx)
	if err != nil {
		c.logf("certificate registry: %v", err)
		return
	}
	for _, e := range entries {
		if !running[e.Principal] {
			kind, name, _ := strings.Cut(e.Principal, ":")
			c.unregisterCert(kind, name)
		}
	}
}
