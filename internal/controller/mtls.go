package controller

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/inbox"
)

// peerKey carries the verified Principal of an mTLS request.
type peerKey struct{}

func peerOf(r *http.Request) (Principal, bool) {
	p, ok := r.Context().Value(peerKey{}).(Principal)
	return p, ok
}

// MTLSHandler is the Controller's mTLS listener (A12S_CONTROLLER, A12S_API;
// enhancements §5.3): identity comes from the client certificate's typed
// URI SAN and counts only while its serial is the live one in the
// registry. The ACL is by principal type: Computers and Services may enqueue
// to the Computers their active generation grants, and read or withdraw
// their own Messages; leases stay Computer-only; nothing else exists here.
func (c *Controller) MTLSHandler(leases http.Handler) http.Handler {
	mux := http.NewServeMux()
	computerOnly := func(h http.Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if p, _ := peerOf(r); p.Kind != KindComputer {
				writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden, Message: p.String() + " may not use leases", Fix: "leases are for Computers"})
				return
			}
			h.ServeHTTP(w, r)
		}
	}
	mux.HandleFunc("POST /v1/leases", computerOnly(leases))
	mux.HandleFunc("DELETE /v1/leases", computerOnly(leases))
	mux.HandleFunc("DELETE /v1/leases/{id}", computerOnly(leases))
	mux.HandleFunc("POST /v1/computers/{c}/inbox", c.granted(c.handleEnqueue))
	mux.HandleFunc("GET /v1/computers/{c}/inbox/{id}", c.ownMessages(c.handleInboxGet))
	mux.HandleFunc("DELETE /v1/computers/{c}/inbox/{id}", c.ownMessages(c.handleWithdraw))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p, _ := peerOf(r)
		writeJSON(w, http.StatusForbidden, api.Error{Code: api.ErrForbidden,
			Message: fmt.Sprintf("%s may not call %s %s on the mTLS listener", p, r.Method, r.URL.Path),
			Fix:     "the mTLS listener serves inbox sends (with a grant) and leases only; owner calls go to the owner API"})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
}

// livePeer verifies the request's certificate against the registry.
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

// recordDenial audits a refused call by a Service, or journals it as a
// diagnostic in the Computer that made it.
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

// registerCert makes a fresh certificate the live one for a Computer or
// Service at start, with its active generation's send grants.
func (c *Controller) registerCert(ctx context.Context, p Principal, serial string, send []string) error {
	if c.inbox == nil {
		return nil
	}
	return c.inbox.RegisterCert(ctx, inbox.CertEntry{Principal: p.String(), Generation: p.Generation, Serial: serial, Send: send})
}

// unregisterCert deletes a principal's live certificate before its
// container is stopped, removed or restarted.
func (c *Controller) unregisterCert(kind, name string) {
	if c.inbox == nil {
		return
	}
	if err := c.inbox.UnregisterCert(context.WithoutCancel(c.ctx), kind+":"+name); err != nil {
		c.logf("%s:%s: unregister certificate: %v", kind, name, err)
	}
}
