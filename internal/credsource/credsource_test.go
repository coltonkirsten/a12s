package credsource

import (
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver/gce/gceapi"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

const secret = "projects/p-123456/secrets/anthropic"

// fakeGCP is the metadata server and Secret Manager: answer is what an access of latest returns.
func fakeGCP(t *testing.T, answer func(w http.ResponseWriter)) *GCP {
	t.Helper()
	tokens := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/token":
			if r.Header.Get("Metadata-Flavor") != "Google" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			tokens++
			fmt.Fprint(w, `{"access_token":"vm-token","expires_in":3600}`)
		case r.URL.Path == "/v1/"+secret+"/versions/latest:access":
			if r.Header.Get("Authorization") != "Bearer vm-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			answer(w)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &GCP{HTTP: srv.Client(), Endpoint: srv.URL + "/v1/", Token: gceapi.MetadataToken(srv.Client(), srv.URL+"/computeMetadata/v1/")}
}

func version(data string, crc string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"name":"projects/123/secrets/anthropic/versions/7","payload":{"data":%q,"dataCrc32c":%q}}`,
			base64.StdEncoding.EncodeToString([]byte(data)), crc)
	}
}

func crcOf(s string) string {
	return fmt.Sprint(crc32.Checksum([]byte(s), crc32.MakeTable(crc32.Castagnoli)))
}

func googleError(status int, code, msg string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"status":%q}}`, status, msg, code)
	}
}

func TestGCPResolve(t *testing.T) {
	const value = "sk-ant-oat01-SECRET-VALUE"
	src := fleet.CredentialSource{GCPSecret: secret}

	got, err := fakeGCP(t, version(value+"\n", crcOf(value+"\n"))).Resolve(t.Context(), src)
	if err != nil || string(got.Value) != value || got.Version != "projects/123/secrets/anthropic/versions/7" {
		t.Fatalf("resolve = %q %q %v (a trailing newline from the console or gcloud is trimmed)", got.Value, got.Version, err)
	}

	unquoted := func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"name":"projects/123/secrets/anthropic/versions/8","payload":{"data":%q,"dataCrc32c":%s}}`,
			base64.StdEncoding.EncodeToString([]byte(value)), crcOf(value))
	}
	if got, err := fakeGCP(t, unquoted).Resolve(t.Context(), src); err != nil || string(got.Value) != value {
		t.Fatalf("a checksum as a JSON number: %v", err)
	}

	for _, tc := range []struct {
		name    string
		answer  func(w http.ResponseWriter)
		noValue bool
	}{
		{"disabled latest", googleError(400, "FAILED_PRECONDITION", "Secret Version [projects/123/secrets/anthropic/versions/7] is in DISABLED state."), true},
		{"destroyed latest", googleError(400, "FAILED_PRECONDITION", "Secret Version [projects/123/secrets/anthropic/versions/7] is in DESTROYED state."), true},
		{"no secret or version", googleError(404, "NOT_FOUND", "Secret [projects/123/secrets/anthropic] not found or has no versions."), true},
		{"empty version", version("  \n", crcOf("  \n")), true},
		{"no access", googleError(403, "PERMISSION_DENIED", "Permission 'secretmanager.versions.access' denied"), false},
		{"outage", googleError(503, "UNAVAILABLE", "The service is currently unavailable."), false},
		{"corrupted", version(value, "12345"), false},
	} {
		_, err := fakeGCP(t, tc.answer).Resolve(t.Context(), src)
		if err == nil || errors.Is(err, ErrNoValue) != tc.noValue {
			t.Errorf("%s: err = %v, want no value %v", tc.name, err, tc.noValue)
		}
		if err != nil && strings.Contains(err.Error(), value) {
			t.Errorf("%s: the error holds the value", tc.name)
		}
	}
}

// Off GCE there is no metadata server: the read fails, and says why, without dropping a value.
func TestGCPResolveOffGCE(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	g := &GCP{HTTP: srv.Client(), Endpoint: srv.URL + "/v1/", Token: gceapi.MetadataToken(srv.Client(), srv.URL+"/computeMetadata/v1/")}
	_, err := g.Resolve(t.Context(), fleet.CredentialSource{GCPSecret: secret})
	if err == nil || errors.Is(err, ErrNoValue) || !strings.Contains(err.Error(), "GCE VM") {
		t.Fatalf("err = %v", err)
	}
}
