package controller

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	l := NewLeases()
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
}
