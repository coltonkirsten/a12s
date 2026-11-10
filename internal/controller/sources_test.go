package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/credsource"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// fakeSecrets is a secret store whose latest version a test sets.
type fakeSecrets struct {
	mu      sync.Mutex
	value   map[string]string
	version map[string]int
	err     map[string]error
	reads   int
}

func (f *fakeSecrets) addVersion(secret, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.value == nil {
		f.value, f.version, f.err = map[string]string{}, map[string]int{}, map[string]error{}
	}
	f.value[secret] = value
	f.version[secret]++
	delete(f.err, secret)
}

func (f *fakeSecrets) fail(secret string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err[secret] = err
}

func (f *fakeSecrets) Resolve(_ context.Context, src fleet.CredentialSource) (credsource.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if err := f.err[src.GCPSecret]; err != nil {
		return credsource.Secret{}, err
	}
	v, ok := f.value[src.GCPSecret]
	if !ok {
		return credsource.Secret{}, fmt.Errorf("%s: NOT_FOUND: %w", src.GCPSecret, credsource.ErrNoValue)
	}
	return credsource.Secret{Value: []byte(v), Version: fmt.Sprintf("%s/versions/%d", src.GCPSecret, f.version[src.GCPSecret])}, nil
}

const labSecret = "projects/lab-project/secrets/lab-anthropic"

func (h *harness) eventually(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			h.t.Fatalf("%s did not happen\nlog:\n%s", what, h.log.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) credential(name string) api.CredentialInfo {
	h.t.Helper()
	for _, c := range decodeBody[api.CredentialsResponse](h.t, h.api("GET", "/v1/credentials", "")).Credentials {
		if c.Name == name {
			return c
		}
	}
	h.t.Fatalf("no credential %s", name)
	return api.CredentialInfo{}
}

// A credential the Fleet reads from a source follows the source: its value reaches the agent computers that use it,
// a new version replaces it, a failed read keeps it, and a withdrawn one stops them until the source has a value.
// No value is ever logged, listed or settable by hand.
func TestCredentialFromSource(t *testing.T) {
	const v1, v2, v3 = "sk-ant-oat01-SOURCED-ONE", "sk-ant-oat01-SOURCED-TWO", "sk-ant-oat01-SOURCED-THREE"
	h := newHarness(t)
	h.secrets.addVersion(labSecret, v1)
	f := labFleet(t, func(f *fleet.Fleet) {
		f.Credentials = map[string]fleet.CredentialSource{"anthropic": {GCPSecret: labSecret}}
	})
	others := maps.Clone(labCreds)
	delete(others, "anthropic")
	if _, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f), Credentials: labCreds}, writeOpts{}); err == nil {
		t.Fatal("a write that sets a sourced credential's value was admitted")
	}
	if _, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f), Credentials: others}, writeOpts{}); err != nil {
		t.Fatal(err)
	}
	secretFile := func(name string) string { return string(h.d.file(name, secretsDir+"/anthropic", "1000:1000", "0600")) }

	h.waitPhase("scout", api.PhaseReady)
	h.waitPhase("tester", api.PhaseReady)
	if got := secretFile("scout"); got != v1 {
		t.Fatalf("scout's anthropic = %q", got)
	}
	info := h.credential("anthropic")
	if !info.Set || info.Source == nil || info.Source.GCPSecret != labSecret || info.SourceVersion != labSecret+"/versions/1" || info.SourceError != "" {
		t.Fatalf("anthropic = %+v", info)
	}

	for _, method := range []string{"PUT", "DELETE"} {
		r := h.api(method, "/v1/credentials/anthropic", `{"value":"sk-ant-oat01-by-hand"}`)
		if e := apiError(t, r); r.StatusCode != 409 || !strings.Contains(e.Fix, labSecret) {
			t.Fatalf("%s a sourced credential = %d %+v", method, r.StatusCode, e)
		}
	}

	if err := h.c.SetPower(context.Background(), "scout", api.PowerRequest{State: api.PowerAwake, Credentials: api.Credentials{"anthropic": []byte("sk-ant-oat01-by-hand")}}); err == nil {
		t.Fatal("a wake that sets a sourced credential's value was admitted")
	}

	h.secrets.addVersion(labSecret, v2)
	h.eventually("scout gets version 2", func() bool { return secretFile("scout") == v2 && secretFile("tester") == v2 })
	if v := h.credential("anthropic").SourceVersion; v != labSecret+"/versions/2" {
		t.Fatalf("sourceVersion = %s", v)
	}

	h.secrets.fail(labSecret, errors.New("Secret Manager answered 503 UNAVAILABLE"))
	h.eventually("the failed read shows", func() bool { return h.credential("anthropic").SourceError != "" })
	if info := h.credential("anthropic"); !info.Set || h.phase("scout") != api.PhaseReady {
		t.Fatalf("a failed read dropped the value: %+v, scout %s", info, h.phase("scout"))
	}

	h.secrets.fail(labSecret, fmt.Errorf("%s/versions/latest is in DISABLED state: %w", labSecret, credsource.ErrNoValue))
	h.waitPhase("scout", api.PhaseProvisioning)
	h.waitPhase("tester", api.PhaseProvisioning)
	if h.d.get("scout").running || h.d.get("tester").running {
		t.Fatal("an agent computer kept running with a withdrawn credential")
	}
	cs := h.c.computerStatus(context.Background(), "scout")
	if cs.Waiting == nil || cs.Waiting.Fix != "add an enabled version to "+labSecret {
		t.Fatalf("scout waits = %+v", cs.Waiting)
	}
	if info := h.credential("anthropic"); info.Set {
		t.Fatalf("withdrawn credential still set: %+v", info)
	}
	if h.phase("builder") != api.PhaseReady {
		t.Fatalf("builder, which doesn't use it, is %s", h.phase("builder"))
	}

	h.secrets.addVersion(labSecret, v3)
	h.waitPhase("scout", api.PhaseReady)
	if got := secretFile("scout"); got != v3 {
		t.Fatalf("scout's anthropic = %q", got)
	}

	// Removing the source from the Fleet makes it a credential set by hand again; it keeps the value it has.
	f.Credentials = nil
	if _, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f)}, writeOpts{}); err != nil {
		t.Fatal(err)
	}
	if info := h.credential("anthropic"); !info.Set || info.Source != nil {
		t.Fatalf("after removing the source: %+v", info)
	}
	if r := h.api("PUT", "/v1/credentials/anthropic", `{"value":"sk-ant-oat01-by-hand"}`); r.StatusCode != 200 {
		t.Fatalf("set by hand = %d", r.StatusCode)
	}

	list, _ := json.Marshal(decodeBody[api.CredentialsResponse](t, h.api("GET", "/v1/credentials", "")))
	h.stop()
	logs := h.log.String()
	stateFiles, _ := os.ReadFile(filepath.Join(h.dir, stateFile))
	for _, v := range []string{v1, v2, v3, "SOURCED"} {
		if strings.Contains(logs, v) || strings.Contains(string(stateFiles), v) || strings.Contains(string(list), v) {
			t.Fatalf("a sourced value appears in the log, %s or the credential list", stateFile)
		}
	}
	if !strings.Contains(logs, "credential anthropic: value from "+labSecret+"/versions/2") {
		t.Fatalf("log doesn't name the version in use:\n%s", logs)
	}
}
