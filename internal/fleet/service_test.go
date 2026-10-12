package fleet

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const svcFleet = `apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: lab }
computers:
  scout:
    harness: { kind: claude-code, version: 2.1.280 }
    image: slim
    uses: [db, { name: pg, mcp: false }]
  builder:
    harness: { kind: codex, version: 0.154.0 }
    image: slim
    grants: { send: [scout] }
services:
  clock:
    image: a12s/svc-cron:0.1
    grants: { send: [scout, builder] }
    config:
      jobs:
        - { name: a, cron: "* * * * *", to: scout, body: hi }
  pg:
    image: postgres:17
    manifest:
      ports: { db: { port: 5432, protocol: tcp } }
      health: { exec: ["pg_isready", "-U", "postgres"] }
      state: { at: /var/lib/postgresql/data, size: 5Gi }
      tmpfs: [/var/run/postgresql]
      user: 999
    credentials: [postgres]
  db:
    image: ghcr.io/coltonkirsten/a12s/svc-mcp-postgres:0.1
    credentials: [postgres]
    uses: [pg]
  hooks:
    image: a12s/svc-webhook:0.1
    grants: { send: [builder] }
    publish: ["127.0.0.1:8787:8080"]
`

func TestServicesParse(t *testing.T) {
	f, err := Parse([]byte(svcFleet))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.ServiceNames(); strings.Join(got, ",") != "clock,db,hooks,pg" {
		t.Fatalf("services %v", got)
	}
	pg := f.Services["pg"]
	if pg.Manifest == nil || pg.Manifest.User.UID != 999 || pg.Manifest.User.GID != 999 || pg.Manifest.State.Size != 5<<30 {
		t.Fatalf("pg manifest %+v", pg.Manifest)
	}
	uses := f.Computers["scout"].Uses
	if len(uses) != 2 || uses[0].Name != "db" || !uses[0].WantsMCP() || uses[1].Name != "pg" || uses[1].WantsMCP() {
		t.Fatalf("uses %+v", uses)
	}
	if f.Computers["builder"].Grants.Send[0] != "scout" {
		t.Fatal("builder grants")
	}
	jobs := f.Services["clock"].Config["jobs"].([]any)
	if jobs[0].(map[string]any)["to"] != "scout" {
		t.Fatalf("config %v", jobs)
	}
	// The Fleet travels to the Controller as JSON: it must survive the trip.
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back Fleet
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("after JSON: %v", err)
	}
	if back.Computers["scout"].Uses[1].WantsMCP() || back.Services["pg"].Manifest.User.UID != 999 {
		t.Fatalf("JSON round trip lost fields: %+v", back.Computers["scout"].Uses)
	}
	if _, err := yaml.Marshal(f); err != nil {
		t.Fatal(err)
	}
}

func TestServiceAdmissionErrors(t *testing.T) {
	// Each refusal names its field path and the offending value; the prose
	// and fix text belong to the error table, not this test.
	cases := []struct{ name, edit, field, value string }{
		{"inbox on a service", "  clock2:\n    image: x/y:1\n    inbox: { maxMessages: 5 }\n", "services.clock2.inbox", "no inbox"},
		{"power on a service", "  clock2:\n    image: x/y:1\n    power: { sleepAfterIdle: 15m }\n", "services.clock2.power", "no power policy"},
		{"harness on a service", "  clock2:\n    image: x/y:1\n    harness: { kind: codex }\n", "services.clock2.harness", "no harness"},
		{"tier image", "  clock2:\n    image: slim\n", "services.clock2.image", `"slim"`},
		{"name clash", "  scout:\n    image: x/y:1\n", "services.scout", "already a Computer"},
		{"grant to a service", "  clock2:\n    image: x/y:1\n    grants: { send: [pg] }\n", "services.clock2.grants.send", "pg is a Service"},
		{"grant to nobody", "  clock2:\n    image: x/y:1\n    grants: { send: [ghost] }\n", "services.clock2.grants.send", "no Computer ghost"},
		{"grant wildcard", "  clock2:\n    image: x/y:1\n    grants: { send: [\"*\"] }\n", "services.clock2.grants.send", `"*"`},
		{"uses itself", "  clock2:\n    image: x/y:1\n    uses: [clock2]\n", "services.clock2.uses", "itself"},
		{"uses unknown", "  clock2:\n    image: x/y:1\n    uses: [ghost]\n", "fix: add services.ghost or remove it from services.clock2.uses", "no Service ghost"},
		{"uses a computer", "  clock2:\n    image: x/y:1\n    uses: [scout]\n", "services.clock2.uses", "scout is a Computer"},
		{"bad publish", "  clock2:\n    image: x/y:1\n    publish: [\"8787:8080\"]\n", "services.clock2.publish", `"8787:8080"`},
		{"manifest user 0", "  clock2:\n    image: x/y:1\n    manifest: { user: 0 }\n", "services.clock2.manifest.user", "root"},
		{"state over /tmp", "  clock2:\n    image: x/y:1\n    manifest: { state: { at: /tmp/x } }\n", "services.clock2.manifest.state.at", "/tmp"},
		{"unknown field", "  clock2:\n    image: x/y:1\n    surprise: 1\n", "services.clock2", "surprise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(svcFleet + tc.edit))
			if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), tc.value) {
				t.Fatalf("got %v, want %s refused (%s)", err, tc.field, tc.value)
			}
		})
	}
	doc := strings.Replace(svcFleet, "    grants: { send: [scout] }\n", "    grants: { send: [scout] }\n    egress: { allow: [], tcp: [{ name: pg, host: db.example.com, port: 5432 }] }\n", 1)
	if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), "computers.builder.egress.tcp: ") {
		t.Fatalf("egress.tcp on a Computer: %v", err)
	}
	// A Computer using a Computer, or an unknown Service.
	doc = strings.Replace(svcFleet, "uses: [db, { name: pg, mcp: false }]", "uses: [ghost]", 1)
	if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), "no Service ghost") || !strings.Contains(err.Error(), "computers.scout.uses") {
		t.Fatalf("unknown use: %v", err)
	}
	// service:<name> in wakeOnMessage resolves against Services.
	doc = strings.Replace(svcFleet, "    uses: [db, { name: pg, mcp: false }]\n", "    uses: [db]\n    power: { wakeOnMessage: { from: [service:clock] } }\n", 1)
	if _, err := Parse([]byte(doc)); err != nil {
		t.Fatalf("service principal: %v", err)
	}
}

func TestValidateServiceManifest(t *testing.T) {
	f, err := Parse([]byte(svcFleet))
	if err != nil {
		t.Fatal(err)
	}
	pg := f.Services["pg"]
	if err := ValidateService("pg", pg, MergeManifest(nil, pg.Manifest), "postgres:17", []string{"/var/lib/postgresql/data"}); err != nil {
		t.Fatalf("pg: %v", err)
	}
	if err := ValidateService("pg", pg, MergeManifest(nil, pg.Manifest), "postgres:17", []string{"/var/lib/postgresql/data/sub"}); err == nil || !strings.Contains(err.Error(), "/var/lib/postgresql/data/sub") {
		t.Fatalf("volume under state: %v", err)
	}
	if err := ValidateService("pg", pg, MergeManifest(nil, pg.Manifest), "postgres:17", []string{"/var/run/postgresql/x"}); err != nil {
		t.Fatalf("volume under tmpfs is covered: %v", err)
	}
	if err := ValidateService("db", f.Services["db"], nil, "ghcr.io/x/y", nil); err == nil || !strings.Contains(err.Error(), "/a12s/service.json") || !strings.Contains(err.Error(), "fix: add services.db.manifest") {
		t.Fatalf("no manifest: %v", err)
	}
	img, err := ParseManifest([]byte(`{"a12s":1,"name":"svc-webhook","version":"0.1.0","user":1000,
"health":{"http":{"port":8080,"path":"/healthz"}},"ports":{"http":{"port":8080}},
"credentials":[{"name":"gh-webhook","required":true}],"login":null}`))
	if err != nil {
		t.Fatal(err)
	}
	hooks := f.Services["hooks"]
	err = ValidateService("hooks", hooks, img, "a12s/svc-webhook:0.1", nil)
	if err == nil || !strings.Contains(err.Error(), "gh-webhook") {
		t.Fatalf("required credential: %v", err)
	}
	hooks.Credentials = []string{"gh-webhook"}
	hooks.Publish = []string{"127.0.0.1:8787:9090"}
	if err := ValidateService("hooks", hooks, img, "a12s/svc-webhook:0.1", nil); err == nil || !strings.Contains(err.Error(), "9090") {
		t.Fatalf("publish undeclared: %v", err)
	}
	// Inline fields override the image's, field by field.
	m := MergeManifest(img, &ServiceManifest{User: &ServiceUser{UID: 2000, GID: 2000}})
	if m.User.UID != 2000 || m.Health.HTTP.Port != 8080 {
		t.Fatalf("merge %+v", m)
	}
	login := &ServiceManifest{Login: &ServiceLogin{Command: []string{"login"}, Produces: "gh-oauth"}}
	hooks.Publish = nil
	if err := ValidateService("hooks", hooks, MergeManifest(img, login), "x", nil); err == nil || !strings.Contains(err.Error(), "gh-oauth") {
		t.Fatalf("login produces: %v", err)
	}
	if _, err := ParseManifest([]byte(`{"a12s":1,"surprise":true}`)); err == nil {
		t.Fatal("unknown manifest field accepted")
	}
}

func TestLockServices(t *testing.T) {
	l := NewLock()
	if !l.PinService("clock", "a12s/svc-cron:0.1", "sha256:a") || l.PinService("clock", "a12s/svc-cron:0.1", "sha256:b") {
		t.Fatal("pin is append-only")
	}
	if l.Services["clock@a12s/svc-cron:0.1"] != "sha256:a" {
		t.Fatalf("%v", l.Services)
	}
}

func TestServicesExampleParses(t *testing.T) {
	f, err := Load("../../examples/services/fleet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Services) != 4 || f.Services["db"].Uses[0].Name != "pg" {
		t.Fatalf("services %v", f.ServiceNames())
	}
}

func TestManifestNumbers(t *testing.T) {
	m, err := ParseManifest([]byte(`{"a12s":1,"user":1000,"resources":{"cpu":0.1,"memory":"64Mi"},"state":{"at":"/d","size":1048576}}`))
	if err != nil || m.Resources.CPU.Millis() != 100 || m.Resources.Memory != 64<<20 || m.State.Size != 1<<20 {
		t.Fatalf("%+v %v", m, err)
	}
}
