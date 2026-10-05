package controller

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/fleet"
)

type leaseClient struct {
	t *testing.T
	h http.Handler
}

func (c leaseClient) do(as, method, target, body string) (int, LeaseResponse) {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if as != "" {
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{DNSNames: []string{as}}}}
	}
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	var res LeaseResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return rec.Code, res
}

func TestLeases(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	l := NewLeases(func(computer, drive string) error {
		if drive != "repo" {
			return errors.New("not shared-write")
		}
		return nil
	})
	l.now = func() time.Time { return now }
	c := leaseClient{t, l.Handler()}
	body := func(computer string) string {
		return `{"computer":"` + computer + `","drive":"repo","path":"src/main.go","ttl":60}`
	}

	code, first := c.do("scout", "POST", "/v1/leases", body("scout"))
	if code != 200 || first.ID == "" || !first.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("acquire = %d %+v", code, first)
	}
	// Another Computer waits until expiry.
	if code, _ := c.do("tester", "POST", "/v1/leases", body("tester")); code != http.StatusConflict {
		t.Fatalf("contended acquire = %d", code)
	}
	// The holder renews: same id, later expiry.
	now = now.Add(30 * time.Second)
	code, renewed := c.do("scout", "POST", "/v1/leases", body("scout"))
	if code != 200 || renewed.ID != first.ID || !renewed.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("renew = %d %+v", code, renewed)
	}
	// A certificate must match the computer in the body.
	if code, _ := c.do("tester", "POST", "/v1/leases", body("scout")); code != http.StatusForbidden {
		t.Fatalf("impersonation = %d", code)
	}
	if code, _ := c.do("", "POST", "/v1/leases", body("scout")); code != http.StatusForbidden {
		t.Fatalf("no certificate = %d", code)
	}
	// Only the holder deletes by id.
	if code, _ := c.do("tester", "DELETE", "/v1/leases/"+first.ID, ""); code != http.StatusForbidden {
		t.Fatalf("foreign delete = %d", code)
	}
	// After expiry another Computer takes it.
	now = now.Add(61 * time.Second)
	code, taken := c.do("tester", "POST", "/v1/leases", body("tester"))
	if code != 200 || taken.ID == first.ID {
		t.Fatalf("after expiry = %d %+v", code, taken)
	}
	if code, _ := c.do("tester", "DELETE", "/v1/leases/"+taken.ID, ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	// computerd's release: DELETE /v1/leases with the same body.
	c.do("scout", "POST", "/v1/leases", body("scout"))
	if code, _ := c.do("scout", "DELETE", "/v1/leases", body("scout")); code != 200 {
		t.Fatalf("release by key = %d", code)
	}
	if code, _ := c.do("tester", "POST", "/v1/leases", body("tester")); code != 200 {
		t.Fatalf("acquire after release = %d", code)
	}
	// The drive must be one the Computer may lock.
	other := `{"computer":"scout","drive":"reference","path":"x"}`
	if code, _ := c.do("scout", "POST", "/v1/leases", other); code != http.StatusForbidden {
		t.Fatalf("unauthorized drive = %d", code)
	}
}

func TestLeaseAuthorization(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, func(f *fleet.Fleet) {
		f.Drives["team"] = fleet.Drive{Access: fleet.AccessSharedWrite}
		for name, ro := range map[string]bool{"scout": false, "reviewer": true} {
			c := f.Computers[name]
			c.Drives = append(c.Drives, fleet.DriveMount{Name: "team", At: "/work/team", ReadOnly: ro})
			f.Computers[name] = c
		}
	}), nil, false)
	for _, tc := range []struct {
		computer, drive string
		ok              bool
	}{
		{"scout", "team", true},
		{"reviewer", "team", false},     // read-only attachment
		{"builder", "team", false},      // not attached
		{"builder", "a12s-repo", false}, // exclusive, not shared-write
		{"scout", "nonexistent", false},
		{"nobody", "team", false},
	} {
		if err := h.c.authorizeLease(tc.computer, tc.drive); (err == nil) != tc.ok {
			t.Errorf("%s on %s: %v, want ok=%v", tc.computer, tc.drive, err, tc.ok)
		}
	}
}
