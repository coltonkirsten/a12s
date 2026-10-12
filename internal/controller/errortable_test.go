package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// TestErrorTable walks the errors with a known fix: each must say what to do, as a `fix:` line in the text or the
// API error's fix field.
func TestErrorTable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	f := withServices(t, h.d, nil)
	h.up(f, nil, false)
	h.waitService("clock", api.PhaseReady)

	parse := func(edit string) string {
		_, err := fleet.Parse([]byte(edit))
		if err == nil {
			return ""
		}
		return err.Error()
	}
	const head = "apiVersion: a12s.ai/v1alpha1\nkind: Fleet\nmetadata: { name: lab }\ncomputers:\n  builder:\n    harness: { kind: codex, version: 0.154.0 }\n    image: slim\n"
	apiErr := func(res *http.Response) string {
		e := apiError(t, res)
		if e.Fix == "" {
			return e.Message
		}
		return e.Message + "  fix: " + e.Fix
	}
	cases := []struct {
		name string
		err  func() string
	}{
		{"maxBytes above the ceiling", func() string { return parse(head + "    inbox: { maxBytes: 2Mi }\n") }},
		{"maxBytes below the floor", func() string { return parse(head + "    inbox: { maxBytes: 4Ki }\n") }},
		{"maxBytes not a size", func() string { return parse(head + "    inbox: { maxBytes: lots }\n") }},
		{"intro too long", func() string { return parse(head + "    inbox: { intro: \"" + strings.Repeat("x", 513) + "\" }\n") }},
		{"inbox on a Service", func() string { return parse(head + "services:\n  clock:\n    image: x/y:1\n    inbox: {}\n") }},
		{"power on a Service", func() string { return parse(head + "services:\n  clock:\n    image: x/y:1\n    power: {}\n") }},
		{"wildcard in wakeOnMessage", func() string { return parse(head + "    power: { wakeOnMessage: { from: [\"*\"] } }\n") }},
		{"unknown Service in uses", func() string { return parse(head + "    uses: [db]\n") }},
		{"grant to a Service", func() string {
			return parse(head + "    grants: { send: [db] }\nservices:\n  db:\n    image: x/y:1\n")
		}},
		{"egress.tcp on a Computer", func() string {
			return parse(head + "    egress: { allow: [], tcp: [{name: a, host: b.example, port: 1}] }\n")
		}},
		{"no manifest in the image", func() string {
			return fleet.ValidateService("db", fleet.Service{Image: "x/y"}, nil, "x/y", nil).Error()
		}},
		{"uncovered VOLUME", func() string {
			m := &fleet.ServiceManifest{User: &fleet.ServiceUser{UID: 9, GID: 9}, Health: &fleet.ServiceHealth{Exec: []string{"true"}}}
			return fleet.ValidateService("pg", fleet.Service{Image: "postgres:17"}, m, "postgres:17", []string{"/data"}).Error()
		}},
		{"credential with no binding", func() string {
			f := withServices(t, h.d, func(f *fleet.Fleet) {
				s := f.Services["clock"]
				s.Credentials = []string{"postgres"}
				f.Services["clock"] = s
			})
			_, err := h.c.PutFleet(ctx, api.PutFleetRequest{Fleet: f, Credentials: labCreds, Presets: labPresets(t)})
			return err.Error()
		}},
		{"needs a login", func() string {
			return h.c.serviceCredentialError("clock", []string{"gh"}).Error() + withLogin(h)
		}},
		{"service without login", func() string {
			_, err := h.c.StartLogin(ctx, "clock")
			var he *httpError
			if errors.As(err, &he) {
				return he.body.Message + "  fix: " + he.body.Fix
			}
			return err.Error()
		}},
		{"the old turns endpoint", func() string { return apiErr(h.api("POST", "/v1/computers/scout/turns", "{}")) }},
		{"a wrong bearer token", func() string {
			return apiErr(h.api("GET", "/v1/computers/scout/inbox", "", "Authorization", "Bearer nope"))
		}},
	}
	for _, c := range cases {
		msg := c.err()
		if msg == "" || !strings.Contains(msg, "fix:") {
			t.Errorf("%s: no fix line in %q", c.name, msg)
		}
	}
}

// withLogin returns "" (serviceCredentialError's text already carries the
// login fix when the manifest declares a login); kept to document the case.
func withLogin(*harness) string { return "" }
