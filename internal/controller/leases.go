package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

const LeasePort = 7443

const (
	defaultLeaseTTL = 60 * time.Second
	maxLeaseTTL     = time.Hour
)

// Leases are in-memory advisory locks: one holder per (drive, path), expiring unless renewed.
type Leases struct {
	now       func() time.Time
	authorize func(computer, drive string) error

	mu    sync.Mutex
	byKey map[leaseKey]*lease
}

type leaseKey struct{ drive, path string }

type lease struct {
	id       string
	computer string
	expires  time.Time
}

type LeaseRequest struct {
	Computer string `json:"computer"`
	Drive    string `json:"drive"`
	Path     string `json:"path"`
	TTL      int    `json:"ttl,omitempty"` // seconds
}

type LeaseResponse struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func NewLeases(authorize func(computer, drive string) error) *Leases {
	return &Leases{now: time.Now, authorize: authorize, byKey: map[leaseKey]*lease{}}
}

// Handler serves the lease API. It must sit behind an mTLS listener: the
// caller's certificate SAN must equal the computer it acts for.
func (l *Leases) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/leases", l.acquire)
	mux.HandleFunc("DELETE /v1/leases/{id}", l.releaseID)
	// computerd releases by (drive, path), as it acquired.
	mux.HandleFunc("DELETE /v1/leases", l.releaseKey)
	return mux
}

func peer(r *http.Request) []string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil
	}
	return r.TLS.PeerCertificates[0].DNSNames
}

func (l *Leases) decode(w http.ResponseWriter, r *http.Request) (LeaseRequest, bool) {
	var req LeaseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "body: "+err.Error())
		return req, false
	}
	if req.Computer == "" || req.Drive == "" || req.Path == "" {
		writeError(w, http.StatusBadRequest, api.ErrInvalid, "computer, drive and path are required")
		return req, false
	}
	if !slices.Contains(peer(r), req.Computer) {
		writeError(w, http.StatusForbidden, api.ErrUnauthorized, "client certificate is not "+req.Computer+"'s")
		return req, false
	}
	if err := l.authorize(req.Computer, req.Drive); err != nil {
		writeError(w, http.StatusForbidden, api.ErrUnauthorized, err.Error())
		return req, false
	}
	req.Path = path.Clean("/" + req.Path)
	return req, true
}

func (l *Leases) acquire(w http.ResponseWriter, r *http.Request) {
	req, ok := l.decode(w, r)
	if !ok {
		return
	}
	ttl := defaultLeaseTTL
	if req.TTL > 0 {
		ttl = min(time.Duration(req.TTL)*time.Second, maxLeaseTTL)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	key := leaseKey{req.Drive, req.Path}
	cur := l.byKey[key]
	if cur != nil && cur.computer != req.Computer {
		writeError(w, http.StatusConflict, api.ErrConflict,
			fmt.Sprintf("%s:%s is held by %s until %s", req.Drive, req.Path, cur.computer, cur.expires.UTC().Format(time.RFC3339)))
		return
	}
	if cur == nil {
		cur = &lease{id: newLeaseID(), computer: req.Computer}
		l.byKey[key] = cur
	}
	cur.expires = now.Add(ttl)
	writeJSON(w, http.StatusOK, LeaseResponse{ID: cur.id, ExpiresAt: cur.expires.UTC()})
}

func (l *Leases) releaseID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(l.now())
	for key, cur := range l.byKey {
		if cur.id != id {
			continue
		}
		if !slices.Contains(peer(r), cur.computer) {
			writeError(w, http.StatusForbidden, api.ErrUnauthorized, "lease belongs to another computer")
			return
		}
		delete(l.byKey, key)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeError(w, http.StatusNotFound, api.ErrNotFound, "no such lease")
}

func (l *Leases) releaseKey(w http.ResponseWriter, r *http.Request) {
	req, ok := l.decode(w, r)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(l.now())
	key := leaseKey{req.Drive, req.Path}
	cur := l.byKey[key]
	if cur == nil || cur.computer != req.Computer {
		writeError(w, http.StatusNotFound, api.ErrNotFound, "no lease held on "+req.Drive+":"+req.Path)
		return
	}
	delete(l.byKey, key)
	writeJSON(w, http.StatusOK, LeaseResponse{ID: cur.id, ExpiresAt: l.now().UTC()})
}

func (l *Leases) sweep(now time.Time) {
	for k, cur := range l.byKey {
		if !now.Before(cur.expires) {
			delete(l.byKey, k)
		}
	}
}

func newLeaseID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "l_" + hex.EncodeToString(b)
}
