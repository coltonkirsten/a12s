package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// A dry run admits a write as the write does, the image's Service manifest and an agent computer's custom image
// included: it never answers OK for a write that then fails.
func TestDryRunAdmitsLikeTheWrite(t *testing.T) {
	h := newHarness(t)
	h.up(withServices(t, h.d, nil), false)
	h.d.mu.Lock()
	h.d.images["t-console-ticker:1"] = "sha256:tickernohealth"
	h.d.svcImages["sha256:tickernohealth"] = driver.ServiceImage{Manifest: []byte(`{"a12s":1,"name":"ticker","version":"1.0.0","user":1000}`), HasCommand: true}
	h.d.computerImages = map[string]driver.ComputerImage{"sha256:plain": {Computerd: true}}
	h.d.mu.Unlock()
	h.d.setImage("me/plain:1", "sha256:plain")
	v0 := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Version

	for _, w := range []struct{ what, path, body, want string }{
		{"service", "/v1/services/ticker", `{"image":"t-console-ticker:1"}`, "services.ticker.manifest.health"},
		{"agent computer", "/v1/agent-computers/hello", `{"harness":{"kind":"claude-code","version":"2.1.280"},"image":"me/plain:1","credentials":["anthropic"]}`, "has no ai.a12s.computerd.version label"},
	} {
		for _, q := range []string{"?dryRun=true", ""} {
			res := h.api("PUT", w.path+q, w.body)
			if e := apiError(t, res); res.StatusCode != 400 || e.Code != api.ErrInvalid || !strings.Contains(e.Message, w.want) {
				t.Errorf("%s PUT%s = %d %s %q, want 400 invalid naming %q", w.what, q, res.StatusCode, e.Code, e.Message, w.want)
			}
		}
	}

	f := withServices(t, h.d, func(f *fleet.Fleet) {
		f.Services["ticker"] = fleet.Service{Image: "t-console-ticker:1"}
	})
	if _, err := h.c.PutFleet(context.Background(), api.PutFleetRequest{Fleet: specFor(f), Credentials: labCreds}, writeOpts{whole: true, dryRun: true}); err == nil || !strings.Contains(err.Error(), "services.ticker.manifest.health") {
		t.Errorf("PUT /v1/fleet dry run: err = %v", err)
	}
	if v := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Version; v != v0 {
		t.Errorf("the refused writes moved the version %d -> %d", v0, v)
	}
}
