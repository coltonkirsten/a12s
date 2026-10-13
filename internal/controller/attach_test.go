package controller

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/driver"
)

const attachApp = `{"project":{"host":"/Users/me/app","keep":["node_modules"]}}`

// An attached folder lets the agent read and write the host, so only the owner sets one: a prefix token can't add,
// change or remove it by any write path, though it may edit everything else of an agent computer the owner gave one.
// A gateway's (action) token can't write agent computers at all.
func TestOnlyTheOwnerAttaches(t *testing.T) {
	h, biz := prefixHarness(t)
	wantAttachForbidden := func(what string, res *http.Response) {
		t.Helper()
		if e := apiError(t, res); res.StatusCode != http.StatusForbidden || e.Code != api.ErrForbidden || !strings.Contains(e.Message, "attach") || e.Fix == "" {
			t.Errorf("%s = %d %s (%s), want 403 forbidden naming attach, with a fix", what, res.StatusCode, e.Code, e.Message)
		}
	}
	wantAttachForbidden("create with attach", biz("PUT", "/v1/agent-computers/biz-a", bizSpec+`,"attach":`+attachApp+`}`))
	wantAttachForbidden("dry-run create with attach", biz("PUT", "/v1/agent-computers/biz-a?dryRun=true", bizSpec+`,"attach":`+attachApp+`}`))
	if r := biz("PUT", "/v1/agent-computers/biz-a", bizSpec+"}"); r.StatusCode != 200 {
		t.Fatalf("create without attach = %d", r.StatusCode)
	}
	wantAttachForbidden("patch attach in", biz("PATCH", "/v1/agent-computers/biz-a", `{"attach":`+attachApp+`}`))

	if r := h.api("PUT", "/v1/agent-computers/biz-s", bizSpec+`,"attach":`+attachApp+`}`); r.StatusCode != 200 {
		t.Fatalf("owner creates biz-s with attach = %d %+v", r.StatusCode, apiError(t, r))
	}
	v := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Version
	wantAttachForbidden("point it elsewhere", biz("PATCH", "/v1/agent-computers/biz-s", `{"attach":{"project":{"host":"/Users/me"}}}`))
	wantAttachForbidden("make it writable elsewhere", biz("PATCH", "/v1/agent-computers/biz-s", `{"attach":{"other":{"host":"/etc"}}}`))
	wantAttachForbidden("drop a kept path", biz("PATCH", "/v1/agent-computers/biz-s", `{"attach":{"project":{"keep":null}}}`))
	wantAttachForbidden("patch it away", biz("PATCH", "/v1/agent-computers/biz-s", `{"attach":null}`))
	wantAttachForbidden("put without it", biz("PUT", "/v1/agent-computers/biz-s", bizSpec+"}"))
	if got := decodeBody[api.FleetResponse](t, h.api("GET", "/v1/fleet", "")).Version; got != v {
		t.Fatalf("refused writes changed the Fleet: v%d -> v%d", v, got)
	}
	if r := biz("PATCH", "/v1/agent-computers/biz-s", `{"instructions":"sell"}`); r.StatusCode != 200 {
		t.Fatalf("patch other fields of biz-s = %d %+v", r.StatusCode, apiError(t, r))
	}
	if r := biz("DELETE", "/v1/agent-computers/biz-s", ""); r.StatusCode != 200 {
		t.Fatalf("remove biz-s = %d", r.StatusCode)
	}

	gw := decodeBody[api.Token](t, h.api("POST", "/v1/tokens", `{"name":"gateway-7012","actions":["status","events","send","inbox"],"computers":["*"]}`))
	r := h.api("PUT", "/v1/agent-computers/biz-g", bizSpec+`,"attach":`+attachApp+`}`, "Authorization", "Bearer "+gw.Secret)
	wantForbidden(t, "gateway token writes attach", r)
}

// The API takes absolute paths only: a relative or ~ path means a Fleet file's directory, which only a12s up knows.
func TestAttachHostMustBeAbsoluteInTheAPI(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	for _, host := range []string{"../app", "~/app"} {
		r := h.api("PUT", "/v1/agent-computers/w", bizSpec+`,"attach":{"p":{"host":"`+host+`"}}}`)
		if e := apiError(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(e.Message, "agent_computers.w.attach.p.host") || !strings.Contains(e.Fix, "absolute") {
			t.Errorf("%s = %d %+v, want 400 asking for an absolute path", host, r.StatusCode, e)
		}
	}
	r := h.api("PUT", "/v1/agent-computers/w", bizSpec+`,"attach":{"p":{"host":"/var/run"}}}`)
	if e := apiError(t, r); r.StatusCode != http.StatusBadRequest || !strings.Contains(e.Message, "Docker socket") {
		t.Errorf("/var/run = %d %+v, want 400", r.StatusCode, e)
	}
}

// attach reaches the Driver as bind mounts plus one keep drive per kept path; changing it is a new generation, and a
// kept path dropped from the Fleet leaves its drive behind as an orphan, like a home drive.
func TestAttachStagesMountsAndKeepDrives(t *testing.T) {
	h := newHarness(t)
	h.up(labFleet(t, nil), false)
	if r := h.api("PUT", "/v1/agent-computers/w", bizSpec+`,"attach":`+attachApp+`}`); r.StatusCode != 200 {
		t.Fatalf("create = %d %+v", r.StatusCode, apiError(t, r))
	}
	keep := driver.KeepDriveID("w", "project", "node_modules")
	spec := h.d.get("w").spec
	want := []driver.Attach{{Host: "/Users/me/app", Target: "/home/agent/project", Keep: []driver.Keep{{Drive: keep, Target: "/home/agent/project/node_modules"}}}}
	if !slices.EqualFunc(spec.Attach, want, func(a, b driver.Attach) bool {
		return a.Host == b.Host && a.Target == b.Target && a.ReadOnly == b.ReadOnly && slices.Equal(a.Keep, b.Keep)
	}) {
		t.Fatalf("attach = %+v, want %+v", spec.Attach, want)
	}
	if !h.d.hasDrive(keep) {
		t.Fatalf("keep drive %s not created", keep)
	}
	drives := decodeBody[api.DrivesResponse](t, h.api("GET", "/v1/drives", "")).Drives
	i := slices.IndexFunc(drives, func(d api.DriveRow) bool { return d.Name == keep })
	if i < 0 || drives[i].Kind != api.DriveKeep || drives[i].Computer != "w" || drives[i].Path != "/home/agent/project/node_modules" || drives[i].Orphaned {
		t.Fatalf("drives = %+v", drives)
	}

	gen := spec.Generation
	if r := h.api("PATCH", "/v1/agent-computers/w", `{"attach":{"project":{"keep":null,"readOnly":true}}}`); r.StatusCode != 200 {
		t.Fatalf("patch = %d %+v", r.StatusCode, apiError(t, r))
	}
	spec = h.d.get("w").spec
	if spec.Generation == gen || len(spec.Attach) != 1 || !spec.Attach[0].ReadOnly || len(spec.Attach[0].Keep) != 0 {
		t.Fatalf("after the patch: generation %d -> %d, attach %+v", gen, spec.Generation, spec.Attach)
	}
	drives = decodeBody[api.DrivesResponse](t, h.api("GET", "/v1/drives", "")).Drives
	if i := slices.IndexFunc(drives, func(d api.DriveRow) bool { return d.Name == keep }); i < 0 || !drives[i].Orphaned || len(drives[i].AttachedTo) > 0 {
		t.Fatalf("a dropped kept path's drive stays, orphaned: %+v", drives)
	}
	if r := h.api("DELETE", "/v1/drives/"+keep, ""); r.StatusCode != http.StatusNoContent {
		t.Fatalf("drive rm %s = %d %+v", keep, r.StatusCode, apiError(t, r))
	}
	if h.d.hasDrive(keep) {
		t.Fatal("keep drive still there")
	}
}
