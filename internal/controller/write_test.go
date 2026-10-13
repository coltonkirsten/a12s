package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

const helloSpec = `{"harness":{"kind":"claude-code","version":"2.1.280"},"image":"slim","credentials":["anthropic"],"instructions":"Be helpful"}`

func TestAgentComputerWrites(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	v0 := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Version

	// A dry run lists the change and changes nothing.
	dry := decodeBody[api.PutFleetResponse](t, h.api("PUT", "/v1/agent-computers/hello?dryRun=true", helloSpec))
	if !dry.DryRun || len(dry.Changes) != 1 || dry.Changes[0].Op != api.ChangeCreate || dry.Version != v0 {
		t.Fatalf("dry run = %+v", dry)
	}
	if res := h.api("GET", "/v1/agent-computers/hello", ""); res.StatusCode != 404 {
		t.Fatalf("dry run created hello: %d", res.StatusCode)
	}

	res := decodeBody[api.PutFleetResponse](t, h.api("PUT", "/v1/agent-computers/hello", helloSpec, "If-None-Match", "*"))
	if res.Version != v0+1 || res.AgentComputer == nil || res.AgentComputer.Status.Phase != api.PhaseReady {
		t.Fatalf("create = %+v", res)
	}
	if got := h.d.get("hello").spec; got.PresetDigest == "" {
		t.Fatal("hello's instructions were not written to its home drive")
	}
	if r := h.api("PUT", "/v1/agent-computers/hello", helloSpec, "If-None-Match", "*"); r.StatusCode != 409 || apiError(t, r).Code != api.ErrExists {
		t.Fatalf("create on a taken name = %d", r.StatusCode)
	}

	// Defaults fill what the spec leaves out; the spec reads back as written.
	ac := decodeBody[api.AgentComputer](t, h.api("GET", "/v1/agent-computers/hello", ""))
	var eff fleet.Computer
	if err := json.Unmarshal(ac.Effective, &eff); err != nil || eff.Resources.Memory == 0 || eff.Instructions != "Be helpful" {
		t.Fatalf("effective = %s (%v)", ac.Effective, err)
	}
	if strings.Contains(string(ac.Spec), "resources") {
		t.Fatalf("spec = %s", ac.Spec)
	}

	// A patch touches only hello; the version moves once per change.
	scoutGen := h.d.get("scout").spec.Generation
	p := decodeBody[api.PutFleetResponse](t, h.api("PATCH", "/v1/agent-computers/hello", `{"instructions":"Be brief","egress":{"allow":["github.com"]}}`))
	if p.Version != v0+2 || len(p.Changes) != 1 || strings.Join(p.Changes[0].Fields, ",") != "egress,instructions" {
		t.Fatalf("patch = %+v", p)
	}
	if h.d.get("scout").spec.Generation != scoutGen {
		t.Fatal("a write to hello touched scout")
	}
	if same := decodeBody[api.PutFleetResponse](t, h.api("PATCH", "/v1/agent-computers/hello", `{"instructions":"Be brief"}`)); same.Version != v0+2 || len(same.Changes) != 0 {
		t.Fatalf("a no-op patch = %+v", same)
	}

	// Listing and the old path both see it.
	list := decodeBody[api.AgentComputersResponse](t, h.api("GET", "/v1/agent-computers", ""))
	if len(list.AgentComputers) != 5 {
		t.Fatalf("list = %d", len(list.AgentComputers))
	}
	if r := h.api("GET", "/v1/computers/hello", ""); r.StatusCode != 200 {
		t.Fatalf("old path = %d", r.StatusCode)
	}

	if d := decodeBody[api.PutFleetResponse](t, h.api("DELETE", "/v1/agent-computers/hello", "")); len(d.Removed) != 1 || d.Version != v0+3 {
		t.Fatalf("delete = %+v", d)
	}
	if h.d.get("hello").spec.Name != "" || !h.d.hasDrive("home-hello") {
		t.Fatal("delete removes the agent computer and keeps its drive")
	}
	if r := h.api("DELETE", "/v1/agent-computers/hello", ""); r.StatusCode != 404 {
		t.Fatalf("delete twice = %d", r.StatusCode)
	}
}

func TestAgentComputerAdmissionCodes(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	for _, tc := range []struct {
		body   string
		status int
		code   api.ErrorCode
	}{
		{`{"image":"slim"}`, 400, api.ErrMissingField},
		{`{"harness":{"kind":"claude-code","version":"2.1.280"},"image":"slim","home":{"preset":"./p"}}`, 400, api.ErrInvalid},
		{`{"harness":{"kind":"claude-code","version":"2.1.280"},"image":"slim","bogus":1}`, 400, api.ErrInvalid},
		{`{"harness":{"kind":"claude-code","version":"2.1.280"},"image":"slim","files":{"../x":"y"}}`, 400, api.ErrInvalid},
	} {
		r := h.api("PUT", "/v1/agent-computers/x", tc.body)
		if e := apiError(t, r); r.StatusCode != tc.status || e.Code != tc.code {
			t.Errorf("%s: %d %s (%s), want %d %s", tc.body, r.StatusCode, e.Code, e.Message, tc.status, tc.code)
		}
	}
}

// Naming a credential with no value admits the agent computer, which waits in Provisioning until the value is set.
func TestCreateWaitsForCredential(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	res := decodeBody[api.PutFleetResponse](t, h.api("PUT", "/v1/agent-computers/job-1",
		`{"harness":{"kind":"claude-code","version":"2.1.280"},"image":"slim","credentials":["anthropic","dbname"]}`))
	want := "agent_computers.job-1: credential dbname has no value yet; it starts when you set it (a12s creds set dbname)"
	if !slices.Contains(res.Warnings, want) || len(res.Errors) != 0 || !slices.Contains(res.Created, "job-1") {
		t.Fatalf("create = %+v", res)
	}
	cs := res.AgentComputer.Status
	if cs.Phase != api.PhaseProvisioning || cs.Error != nil || cs.Waiting == nil ||
		!slices.Equal(cs.Waiting.Credentials, []string{"dbname"}) || cs.Waiting.Fix != "a12s creds set dbname" {
		t.Fatalf("status = %+v", cs)
	}
	if h.d.get("job-1").running {
		t.Fatal("started without its credential")
	}
	if r := h.api("POST", "/v1/agent-computers/job-1/inbox", `{"body":"hi"}`); r.StatusCode >= 300 {
		t.Fatalf("send while waiting = %d %s", r.StatusCode, apiError(t, r).Message)
	}

	if r := h.api("PUT", "/v1/credentials/dbname", `{"value":"db.example"}`); r.StatusCode != 200 {
		t.Fatalf("set = %d", r.StatusCode)
	}
	h.waitPhase("job-1", api.PhaseReady)
	if cs := h.c.computerStatus(context.Background(), "job-1"); cs.Waiting != nil {
		t.Fatalf("still waiting: %+v", cs.Waiting)
	}
	if got := h.d.file("job-1", secretsDir+"/dbname", "1000:1000", "0600"); string(got) != "db.example" {
		t.Fatalf("dbname = %q", got)
	}
}

func TestFleetIfMatch(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	fr := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", ""))
	body, _ := json.Marshal(api.PutFleetRequest{Fleet: &fr.Fleet, Credentials: labCreds})
	stale := `"` + string(rune('0'+fr.Version-1)) + `"`
	if r := h.api("PUT", "/v1/fleet", string(body), "If-Match", stale); r.StatusCode != http.StatusPreconditionFailed || apiError(t, r).Code != api.ErrVersionMismatch {
		t.Fatalf("stale If-Match = %d", r.StatusCode)
	}
	r := h.api("PUT", "/v1/fleet", string(body), "If-Match", etag(fr.Version))
	if r.StatusCode != 200 || r.Header.Get("ETag") != etag(fr.Version) {
		t.Fatalf("matching If-Match = %d, etag %s (an unchanged Fleet keeps its version)", r.StatusCode, r.Header.Get("ETag"))
	}
}

// A Controller upgraded from a release that stored the merged Fleet keeps every agent computer, with its preset as
// files and its seccomp profile inline.
func TestLegacyStateMigrates(t *testing.T) {
	dir := t.TempDir()
	tarData := mustPresetTar(t)
	sum, err := hashPresetTar(tarData)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, presetDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(presetPath(dir, sum), tarData, 0o600); err != nil {
		t.Fatal(err)
	}
	profile := `{"defaultAction":"SCMP_ACT_ALLOW"}`
	legacy := `{"fleet":{"apiVersion":"a12s.ai/v1alpha1","kind":"Fleet","metadata":{"name":"lab"},
		"computers":{"scout":{"harness":{"kind":"claude-code","version":"2.1.280"},"image":"slim","home":{"preset":"./presets/scout"},"security":{"seccomp":"./p.json"}}}},
		"lock":{"images":{},"bundles":{},"presets":{"./presets/scout":"` + sum + `"},"seccomp":{"./p.json":"sha256:x"}},
		"seccomp":{"sha256:x":` + quoted(profile) + `},"computers":{}}`
	if err := os.WriteFile(filepath.Join(dir, stateFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(filepath.Join(dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	c := st.Fleet.Computers["scout"]
	if c.Home.Preset != "" || !strings.Contains(c.Files["CLAUDE.md"], "scout") || string(c.Security.Seccomp.Profile) != profile {
		t.Fatalf("migrated scout = %+v", c)
	}
}

func mustPresetTar(t *testing.T) []byte {
	t.Helper()
	files, err := fleet.InlineFiles("../../examples/lab/presets/scout")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := fleet.FilesTar(files)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Credentials are stored 0600 in the state volume and survive a restart; nobody reads a value back, and one still
// in use can't be removed.
func TestCredentialStore(t *testing.T) {
	dir := t.TempDir()
	persistCreds[dir] = true
	h := startHarness(t, dir, nil)
	h.up(labFleet(t, nil), false)
	if r := h.api("PUT", "/v1/credentials/hubspot", `{"value":"hs-secret"}`); r.StatusCode != 200 {
		t.Fatalf("set = %d", r.StatusCode)
	}
	info, err := os.Stat(filepath.Join(dir, credsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", info, err)
	}
	list := decodeBody[api.CredentialsResponse](t, h.api("GET", "/v1/credentials", ""))
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), "hs-secret") || len(list.Credentials) != 4 {
		t.Fatalf("list = %s", b)
	}
	if r := h.api("DELETE", "/v1/credentials/github", ""); r.StatusCode != 409 || apiError(t, r).Code != api.ErrInUse {
		t.Fatalf("delete in use = %d", r.StatusCode)
	}
	if r := h.api("DELETE", "/v1/credentials/hubspot", ""); r.StatusCode != 204 {
		t.Fatalf("delete = %d", r.StatusCode)
	}

	h.d.RemoveComputer(context.Background(), "scout")
	h2 := h.restart()
	h2.waitPhase("scout", api.PhaseReady) // restaged and started with the stored anthropic credential
	if st := h2.c.Status(context.Background()); !st.CredentialsLoaded {
		t.Fatal("credentials were not loaded after the restart")
	}
}
