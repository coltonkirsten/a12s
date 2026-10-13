package controller

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
)

// A Controller upgraded from 0.6.0 starts on the Fleet it stored, which may still hold fields since removed, and
// keeps every agent computer and Service running.
func TestStateFromV060Loads(t *testing.T) {
	h := newHarness(t)
	h.up(withServices(t, h.d, nil), false)
	h.waitPhase("scout", api.PhaseReady)
	h.waitService("clock", api.PhaseReady)
	h.stop()

	path := filepath.Join(h.dir, stateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	obj := func(m map[string]any, k string) map[string]any {
		if m[k] == nil {
			m[k] = map[string]any{}
		}
		return m[k].(map[string]any)
	}
	spec := obj(st, "spec")
	spec["drives"] = map[string]any{"scratch": map[string]any{"access": "shared", "size": "1Gi"}}
	spec["egress"] = map[string]any{"enforcement": "unenforced", "allowPrivate": []any{"10.0.0.0/8"}}
	defaults := obj(spec, "defaults")
	defaults["autonomy"] = "full"
	defaults["home"] = map[string]any{"size": "10Gi"}
	defaults["inbox"] = map[string]any{"intro": "New messages:"}
	scout := obj(obj(spec, "agent_computers"), "scout")
	scout["autonomy"] = "restricted"
	scout["home"] = map[string]any{"size": "20Gi"}
	scout["inbox"] = map[string]any{"intro": "", "maxMessages": 50}
	scout["drives"] = []any{map[string]any{"name": "scratch", "at": "/work", "readOnly": true}}
	clock := obj(obj(spec, "services"), "clock")
	clock["publish"] = []any{"127.0.0.1:8080:8080"}
	clock["egress"] = map[string]any{"allow": []any{}, "tcp": []any{map[string]any{"name": "db", "host": "10.0.0.5", "port": 5432}}}
	clock["manifest"] = map[string]any{"login": map[string]any{"command": []any{"login"}, "produces": "token"}}
	obj(obj(st, "manifests"), "clock")["login"] = map[string]any{"command": []any{"login"}, "produces": "token"}
	obj(obj(obj(st, "computers"), "scout"), "active")["drives"] = []any{map[string]any{"name": "scratch", "at": "/work"}}
	if data, err = json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	h2 := startHarness(t, h.dir, h.d)
	for _, name := range []string{"scout", "builder", "tester", "reviewer"} {
		h2.waitPhase(name, api.PhaseReady)
	}
	h2.waitService("clock", api.PhaseReady)
	if strings.Contains(h2.log.String(), "removed") {
		t.Fatalf("something was removed:\n%s", h2.log.String())
	}
	if m := h2.c.state.Fleet.Computers["scout"].Inbox.MaxMessages; m == nil || *m != 50 {
		t.Fatalf("scout's inbox.maxMessages was lost: %v", m)
	}
	if res := h2.api(http.MethodPatch, "/v1/agent-computers/scout", `{"instructions":"You are scout, still."}`); res.StatusCode != http.StatusOK {
		t.Fatalf("patch after upgrade: %d %+v", res.StatusCode, apiError(t, res))
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(saved, &stored); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"autonomy", "intro", "home", "drives", "allowPrivate", "enforcement", "publish", "tcp", "login"} {
		if strings.Contains(string(stored.Spec), `"`+gone+`"`) {
			t.Errorf("stored spec still has %q: %s", gone, stored.Spec)
		}
	}
}
