package fleet

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseLabExample(t *testing.T) {
	f, err := Load("testdata/lab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The contract fields only; TestDefaultsMerge owns the merge rules, and a
	// new defaulted field must not break this test.
	if f.Metadata.Name != "lab" || len(f.Drives) != 3 || f.Drives["reference"].Access != AccessShared || f.Drives["a12s-repo"].Access != AccessExclusive {
		t.Fatalf("fleet %+v drives %+v", f.Metadata, f.Drives)
	}
	if got := f.ComputerNames(); !reflect.DeepEqual(got, []string{"builder", "reviewer", "scout", "tester"}) {
		t.Errorf("ComputerNames = %v", got)
	}
	scout, builder, tester := f.Computers["scout"], f.Computers["builder"], f.Computers["tester"]
	if scout.Harness != (Harness{Kind: HarnessClaudeCode, Version: "2.1.280", Model: "claude-fable-5-1"}) || scout.Home.Preset != "./presets/scout" ||
		!reflect.DeepEqual(scout.Drives, []DriveMount{{Name: "reference", At: "/work/ref", ReadOnly: true}}) ||
		!reflect.DeepEqual(scout.Egress.Allow, []string{"api.anthropic.com"}) || !reflect.DeepEqual(scout.Credentials, []string{"anthropic"}) {
		t.Errorf("scout %+v", scout)
	}
	if builder.Harness.Kind != HarnessCodex || !reflect.DeepEqual(builder.Credentials, []string{"openai", "github"}) || builder.Autonomy != AutonomyFull {
		t.Errorf("builder %+v", builder)
	}
	if tester.Image != TierSlim || tester.Resources.CPU != 2000 || tester.Resources.Memory != 4*Bytes(1<<30) {
		t.Errorf("tester %+v", tester)
	}
}

// base is a minimal valid Fleet; tests append or substitute YAML around it.
const base = `apiVersion: a12s.ai/v1alpha1
kind: Fleet
metadata: { name: t }
`

func TestDefaultsMerge(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want Computer
	}{
		{
			name: "maps merge key by key",
			yaml: `
defaults:
  resources: { cpu: 1, memory: 2Gi }
  harness: { kind: codex, version: 0.154.0 }
  image: slim
computers:
  c:
    resources: { memory: 4Gi }
    harness: { model: gpt-6-astra }
`,
			want: Computer{
				Harness:   Harness{Kind: HarnessCodex, Version: "0.154.0", Model: "gpt-6-astra"},
				Image:     TierSlim,
				Resources: Resources{CPU: 1000, Memory: 4 << 30},
				Autonomy:  AutonomyFull,
			},
		},
		{
			name: "lists replace instead of appending",
			yaml: `
defaults:
  harness: { kind: codex, version: 0.154.0 }
  image: slim
  credentials: [openai]
  egress: { allow: [a.example.com, b.example.com] }
computers:
  c:
    credentials: [github]
    egress: { allow: [c.example.com] }
`,
			want: Computer{
				Harness:     Harness{Kind: HarnessCodex, Version: "0.154.0"},
				Image:       TierSlim,
				Autonomy:    AutonomyFull,
				Credentials: []string{"github"},
				Egress:      Egress{Allow: []string{"c.example.com"}},
			},
		},
		{
			name: "empty list overrides a default list",
			yaml: `
defaults:
  harness: { kind: codex, version: 0.154.0 }
  image: slim
  egress: { allow: [a.example.com] }
computers:
  c: { egress: { allow: [] } }
`,
			want: Computer{
				Harness:  Harness{Kind: HarnessCodex, Version: "0.154.0"},
				Image:    TierSlim,
				Autonomy: AutonomyFull,
				Egress:   Egress{Allow: []string{}},
			},
		},
		{
			name: "scalar overrides and version text is kept",
			yaml: `
defaults:
  harness: { kind: claude-code, version: 1.10 }
  image: slim
  autonomy: full
computers:
  c: { autonomy: restricted, image: me/agent:1 }
`,
			want: Computer{
				Harness:  Harness{Kind: HarnessClaudeCode, Version: "1.10"},
				Image:    "me/agent:1",
				Autonomy: AutonomyRestricted,
			},
		},
		{
			name: "computer with no body inherits everything",
			yaml: `
defaults:
  harness: { kind: codex, version: "1" }
  image: slim
  resources: { cpu: 500m }
computers:
  c:
`,
			want: Computer{
				Harness:   Harness{Kind: HarnessCodex, Version: "1"},
				Image:     TierSlim,
				Resources: Resources{CPU: 500},
				Autonomy:  AutonomyFull,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Parse([]byte(base + tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := f.Computers["c"]; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	const c = `
computers:
  c:
    harness: { kind: codex, version: 0.154.0 }
    image: slim
`
	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring; empty means valid
	}{
		{"minimal", base + c, ""},
		{"bad apiVersion", strings.Replace(base, "a12s.ai/v1alpha1", "v1", 1) + c, "apiVersion"},
		{"bad kind", strings.Replace(base, "Fleet", "Manifest", 1) + c, "kind"},
		{"bad fleet name", strings.Replace(base, "name: t", "name: Lab_1", 1) + c, "metadata.name"},
		{"unknown field", base + c + "    color: blue\n", "field color not found"},
		{"bad computer name", base + strings.Replace(c, "  c:", "  Big:", 1), "computers.Big"},
		{"bad harness kind", base + strings.Replace(c, "codex", "aider", 1), "harness.kind"},
		{"missing version", base + strings.Replace(c, ", version: 0.154.0", "", 1), "harness.version"},
		{"custom image", base + strings.Replace(c, "image: slim", "image: ghcr.io/acme/agent:1.2", 1), ""},
		{"custom image by digest", base + strings.Replace(c, "image: slim", "image: sha256:"+strings.Repeat("a", 64), 1), ""},
		{"bad image", base + strings.Replace(c, "image: slim", "image: 'Not An Image'", 1), "computers.c.image"},
		{"missing image", base + strings.Replace(c, "    image: slim\n", "", 1), "computers.c.image"},
		{"restricted", base + c + "    autonomy: restricted\n", ""},
		{"bad autonomy", base + c + "    autonomy: yolo\n", "autonomy"},
		{"egress hosts", base + c + "    egress: { allow: [api.openai.com, github.com] }\n", ""},
		{"egress star", base + c + "    egress: { allow: ['*'] }\n", ""},
		{"egress url", base + c + "    egress: { allow: ['https://github.com'] }\n", "egress.allow"},
		{"egress port", base + c + "    egress: { allow: ['github.com:22'] }\n", "egress.allow"},
		{"egress wildcard", base + c + "    egress: { allow: ['*.github.com'] }\n", "egress.allow"},
		{"unenforced", base + "egress: { enforcement: unenforced }\n" + c, ""},
		{"bad enforcement", base + "egress: { enforcement: maybe }\n" + c, "egress.enforcement"},
		{"resources", base + c + "    resources: { cpu: 0.5, memory: 512Mi }\n", ""},
		{"bad memory", base + c + "    resources: { memory: 2GB }\n", "invalid size"},
		{"bad cpu", base + c + "    resources: { cpu: lots }\n", "invalid cpu"},
		{"bad home size", base + c + "    home: { size: -1Gi }\n", "invalid size"},
		{"absolute preset", base + c + "    home: { preset: /etc/x }\n", "home.preset"},
		{"credentials", base + c + "    credentials: [openai, github]\n", ""},
		{"duplicate credential", base + c + "    credentials: [openai, openai]\n", "listed twice"},
		{"undeclared drive", base + c + "    drives: [{ name: nope, at: /work/x }]\n", "not declared"},
		{"bad access", base + "drives: { d: { access: public } }\n" + c, "drives.d.access"},
		{"relative mount", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: work }]\n", "absolute"},
		{"reserved mount", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /home/agent/x }]\n", "reserved"},
		{"mount comma", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: '/work/v,/home/.a12s' }]\n", "may only contain"},
		{"mount colon", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: '/work/v:rw' }]\n", "may only contain"},
		{"mount space", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: '/work/my dir' }]\n", "may only contain"},
		{"mount unicode", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /work/café }]\n", "may only contain"},
		{"mount unclean", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /work/../home }]\n", "clean absolute"},
		{"mount trailing slash", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /work/ }]\n", "clean absolute"},
		{"mount root", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: / }]\n", "reserved"},
		{"mount allowed chars", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /work/A-z_0.9 }]\n", ""},
		{"reserved /home", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /home/x }]\n", "reserved"},
		{"reserved /run/a12s", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /run/a12s/x }]\n", "reserved"},
		{"reserved /opt/a12s", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /opt/a12s/x }]\n", "reserved"},
		{"reserved /tmp", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /tmp/x }]\n", "reserved"},
		{"reserved /proc", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /proc/x }]\n", "reserved"},
		{"reserved /sys", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /sys/x }]\n", "reserved"},
		{"reserved /dev", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /dev/x }]\n", "reserved"},
		{"shared readOnly", base + "drives: { d: { access: shared } }\n" + c + "    drives: [{ name: d, at: /w, readOnly: true }]\n", ""},
		{"shared writable", base + "drives: { d: { access: shared } }\n" + c + "    drives: [{ name: d, at: /w }]\n", "must be attached readOnly"},
		{"shared-write writable", base + "drives: { d: { access: shared-write } }\n" + c + "    drives: [{ name: d, at: /w }]\n", ""},
		{"same drive twice", base + "drives: { d: { access: shared } }\n" + c + "    drives: [{ name: d, at: /a, readOnly: true }, { name: d, at: /b, readOnly: true }]\n", "attached twice"},
		{"exclusive on two computers", base + "drives: { d: { access: exclusive } }\n" + c + "    drives: [{ name: d, at: /w }]\n" + `  e:
    harness: { kind: codex, version: 0.154.0 }
    image: slim
    drives: [{ name: d, at: /w, readOnly: true }]
`, "attached to more than one agent computer: c, e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	_, err := Parse([]byte(`apiVersion: v0
kind: Fleet
metadata: { name: t }
computers:
  c: { harness: { kind: x, version: "1" }, image: slim, autonomy: none }
`))
	if err == nil {
		t.Fatal("want error")
	}
	for _, s := range []string{"apiVersion", "harness.kind", "autonomy"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not mention %s", err, s)
		}
	}
}

func TestQuantities(t *testing.T) {
	bytesTests := []struct {
		in   string
		want Bytes
		str  string
	}{
		{"10Gi", 10 << 30, "10Gi"},
		{"512Mi", 512 << 20, "512Mi"},
		{"1.5Gi", 1536 << 20, "1536Mi"},
		{"4Ki", 4096, "4Ki"},
		{"1Ti", 1 << 40, "1Ti"},
		{"1000", 1000, "1000"},
		{"0", 0, "0"},
	}
	for _, tt := range bytesTests {
		got, err := ParseBytes(tt.in)
		if err != nil || got != tt.want || got.String() != tt.str {
			t.Errorf("ParseBytes(%q) = %d (%s), %v; want %d (%s)", tt.in, got, got, err, tt.want, tt.str)
		}
	}
	for _, in := range []string{"", "Gi", "2GB", "-1", "x1Mi", "9999999999Ti"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q): want error", in)
		}
	}
	cpuTests := []struct {
		in   string
		want CPU
		str  string
	}{
		{"1", 1000, "1"},
		{"2", 2000, "2"},
		{"0.5", 500, "500m"},
		{"250m", 250, "250m"},
	}
	for _, tt := range cpuTests {
		got, err := ParseCPU(tt.in)
		if err != nil || got != tt.want || got.String() != tt.str {
			t.Errorf("ParseCPU(%q) = %d (%s), %v; want %d (%s)", tt.in, got, got, err, tt.want, tt.str)
		}
	}
	for _, in := range []string{"", "m", "-1", "one"} {
		if _, err := ParseCPU(in); err == nil {
			t.Errorf("ParseCPU(%q): want error", in)
		}
	}
}

func TestValidateSeccomp(t *testing.T) {
	chromium, err := os.ReadFile("testdata/chromium-seccomp.json")
	if err != nil {
		t.Fatal(err)
	}
	if allowAll, err := ValidateSeccomp(chromium); err != nil || allowAll {
		t.Fatalf("chromium profile: allowAll %v, err %v", allowAll, err)
	}
	if allowAll, err := ValidateSeccomp([]byte(`{"defaultAction":"SCMP_ACT_ALLOW"}`)); err != nil || !allowAll {
		t.Fatalf("allow-by-default profile: allowAll %v, err %v", allowAll, err)
	}
	for _, bad := range []string{
		``,
		`unconfined`,
		`{"defaultAction":"SCMP_ACT_ERRNO"}`,
		`{"defaultAction":"ALLOW_EVERYTHING","syscalls":[{"names":["read"],"action":"SCMP_ACT_ALLOW"}]}`,
		`{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read"],"action":"SCMP_ACT_NOTIFY"}],"listenerPath":"/x"}`,
		`{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read"],"action":"SCMP_ACT_NOTIFY"}]}`,
		`{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"action":"SCMP_ACT_ALLOW"}]}`,
		`{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read"],"action":"SCMP_ACT_ALLOW"}]} {}`,
		`{"defaultAction":"SCMP_ACT_ALLOW","archMap":[{"architecture":"SCMP_ARCH_AARCH64","subArchitectures":["SCMP_ARCH_32"]}]}`,
		`{"defaultAction":"SCMP_ACT_ALLOW","architectures":["x86_64"]}`,
	} {
		if _, err := ValidateSeccomp([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSecurityAndRemovedBrowserTier(t *testing.T) {
	for yaml, want := range map[string]string{
		"image: browser": "no longer part of a12s",
		"image: slim\n    security: { seccomp: /etc/x.json }": "must be a path relative to the Fleet file",
	} {
		_, err := Parse([]byte(base + "computers:\n  c:\n    harness: { kind: claude-code, version: 1.0.0 }\n    autonomy: full\n    " + yaml + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", yaml, err, want)
		}
	}
	f, err := Parse([]byte(base + "defaults:\n  security: { seccomp: ./profiles/chromium.json }\ncomputers:\n  c:\n    harness: { kind: claude-code, version: 1.0.0 }\n    autonomy: full\n    image: me/agent:1\n"))
	if err != nil || f.Computers["c"].Security.Seccomp.Path != "./profiles/chromium.json" {
		t.Fatalf("security from defaults: %v %+v", err, f)
	}
}
