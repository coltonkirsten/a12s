package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

func TestServiceLogin(t *testing.T) {
	h := newHarness(t)
	f := withServices(t, h.d, func(f *fleet.Fleet) {
		s := f.Services["clock"]
		s.Credentials = []string{"gh-oauth"}
		s.Manifest = &fleet.ServiceManifest{Login: &fleet.ServiceLogin{Command: []string{"gh-login", "--device"}, Produces: "gh-oauth"}}
		f.Services["clock"] = s
	})
	h.up(f, nil, false)
	h.waitService("clock", api.PhaseError)
	l, err := h.c.StartLogin(context.Background(), "clock")
	if err != nil {
		t.Fatal(err)
	}
	if l.Produces != "gh-oauth" || !slices.Equal(l.Command, []string{"gh-login", "--device"}) || l.User != "1000:1000" {
		t.Fatalf("login %+v", l)
	}
	h.d.mu.Lock()
	spec := h.d.logins["clock"]
	h.d.mu.Unlock()
	// Memory-only: the state path is scratch tmpfs here, never the drive.
	if spec.Image != "sha256:svctest01aaaaaaaa" || !slices.Contains(spec.Tmpfs, "/var/lib/svc") {
		t.Fatalf("login spec %+v", spec)
	}
	if err := h.c.EndLogin(context.Background(), "clock"); err != nil {
		t.Fatal(err)
	}
	// With the credential bound, up starts the Service with it in tmpfs.
	creds := api.Credentials{"gh-oauth": []byte("gho_123")}
	for k, v := range labCreds {
		creds[k] = v
	}
	if _, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: f, Credentials: creds, Presets: labPresets(t)}); err != nil {
		t.Fatal(err)
	}
	h.waitService("clock", api.PhaseReady)
	s := h.d.svc("clock")
	if string(s.files["/run/a12s/secrets/gh-oauth"]) != "gho_123" || s.owners["/run/a12s/secrets/gh-oauth"] != "1000:1000 0400" {
		t.Fatalf("secret %q %q", s.files["/run/a12s/secrets/gh-oauth"], s.owners["/run/a12s/secrets/gh-oauth"])
	}
	if s.owners["/run/a12s/tls/key.pem"] != "1000:1000 0400" {
		t.Fatalf("tls key %q", s.owners["/run/a12s/tls/key.pem"])
	}
	noSecretsOnDisk(t, h.dir)
	// A manifest without login refuses with a fix.
	f2 := withServices(t, h.d, nil)
	h.up(f2, nil, false)
	if _, err := h.c.StartLogin(context.Background(), "clock"); err == nil || !strings.Contains(err.Error(), "declares no login") {
		t.Fatalf("no login: %v", err)
	}
	_ = driver.LoginOut
}
