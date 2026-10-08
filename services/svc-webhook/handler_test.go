package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/services/svckit"
)

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestHandlerFailsClosed: every refusal returns before the enqueue, so a nil
// client proves nothing was sent.
func TestHandlerFailsClosed(t *testing.T) {
	dir := t.TempDir()
	old := svckit.SecretsDir
	svckit.SecretsDir = dir
	t.Cleanup(func() { svckit.SecretsDir = old })
	for name, v := range map[string]string{"good": "s3cret\n", "empty": "", "blank": " \n\t "} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte(`{"action":"opened"}`)
	cases := []struct {
		name, secret, sig string
		body              []byte
		want              int
	}{
		// An empty key's HMAC is computable by anyone: it must not validate.
		{"empty secret", "empty", sign(nil, body), body, http.StatusServiceUnavailable},
		{"whitespace secret", "blank", sign(nil, body), body, http.StatusServiceUnavailable},
		{"missing secret", "absent", sign([]byte("s3cret"), body), body, http.StatusServiceUnavailable},
		{"bad signature", "good", sign([]byte("other"), body), body, http.StatusUnauthorized},
		{"unsigned", "good", "", body, http.StatusUnauthorized},
		{"too large", "good", "", []byte(strings.Repeat("x", maxPayload+1)), http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := handler(nil, route{Path: "/gh", To: "c1", SecretFrom: c.secret})
			req := httptest.NewRequest("POST", "/gh", strings.NewReader(string(c.body)))
			if c.sig != "" {
				req.Header.Set("X-Hub-Signature-256", c.sig)
			}
			w := httptest.NewRecorder()
			h(w, req)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d (%s)", w.Code, c.want, w.Body)
			}
		})
	}
}

func TestEmptySecretNeverValidates(t *testing.T) {
	body := []byte(`{}`)
	if validSignature(nil, body, sign(nil, body)) {
		t.Fatal("empty key validated")
	}
}
