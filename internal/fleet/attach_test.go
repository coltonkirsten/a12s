package fleet

import (
	"strings"
	"testing"
)

func TestValidateAttach(t *testing.T) {
	const c = "agent_computers:\n  c:\n    harness: { kind: claude-code, version: 1.0.0 }\n    image: slim\n"
	tests := []struct {
		name, yaml, wantErr string
	}{
		{"absolute", "    attach: { project: { host: /Users/me/app } }\n", ""},
		{"relative in a Fleet file", "    attach: { project: { host: ../app, readOnly: true } }\n", ""},
		{"home in a Fleet file", "    attach: { project: { host: ~/app, keep: [node_modules, .venv, packages/web/node_modules] } }\n", ""},
		{"no host", "    attach: { project: { readOnly: true } }\n", "attach.project.host: required"},
		{"bad name", "    attach: { My_App: { host: /x/app } }\n", "attach.My_App: name must be a lowercase DNS label"},
		{"dotfile name", "    attach: { .claude: { host: /x/app } }\n", "DNS label"},
		{"reserved", "    attach: { bin: { host: /x/app } }\n", "reserved"},
		{"shadows files", "    files: { tools/x.sh: hi }\n    attach: { tools: { host: /x/app } }\n", "files or instructions already write tools/"},
		{"root", "    attach: { p: { host: / } }\n", "whole filesystem"},
		{"unclean", "    attach: { p: { host: /x/../app } }\n", "clean absolute path"},
		{"docker socket", "    attach: { p: { host: /var/run/docker.sock } }\n", "Docker socket"},
		{"socket dir", "    attach: { p: { host: /var/run } }\n", "Docker socket"},
		{"socket dir parent", "    attach: { p: { host: /var } }\n", "Docker socket"},
		{"inside run", "    attach: { p: { host: /run/user/1000 } }\n", "Docker socket"},
		{"docker desktop dir", "    attach: { p: { host: /Users/me/.docker } }\n", "Docker socket"},
		{"keep absolute", "    attach: { p: { host: /x/app, keep: [/node_modules] } }\n", "clean path relative"},
		{"keep escapes", "    attach: { p: { host: /x/app, keep: [../x] } }\n", "clean path relative"},
		{"keep unclean", "    attach: { p: { host: /x/app, keep: [a//b] } }\n", "clean path relative"},
		{"keep dot", "    attach: { p: { host: /x/app, keep: [.] } }\n", "clean path relative"},
		{"keep space", "    attach: { p: { host: /x/app, keep: ['my modules'] } }\n", "clean path relative"},
		{"keep twice", "    attach: { p: { host: /x/app, keep: [a, a] } }\n", "listed twice"},
		{"keep read-only", "    attach: { p: { host: /x/app, readOnly: true, keep: [a] } }\n", "needs a writable folder"},
		{"unknown field", "    attach: { p: { host: /x/app, mode: rw } }\n", "field mode not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(base + c + tt.yaml))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateAttachLimits(t *testing.T) {
	var many, keep strings.Builder
	for i := range MaxAttach + 1 {
		many.WriteString("      p" + string(rune('a'+i)) + ": { host: /x/app }\n")
	}
	for i := range MaxKeep + 1 {
		keep.WriteString("k" + string(rune('a'+i)) + ",")
	}
	const c = "agent_computers:\n  c:\n    harness: { kind: claude-code, version: 1.0.0 }\n    image: slim\n    attach:\n"
	if _, err := Parse([]byte(base + c + many.String())); err == nil || !strings.Contains(err.Error(), "more than 8") {
		t.Errorf("too many folders: %v", err)
	}
	if _, err := Parse([]byte(base + c + "      p: { host: /x/app, keep: [" + strings.TrimSuffix(keep.String(), ",") + "] }\n")); err == nil || !strings.Contains(err.Error(), "more than 16") {
		t.Errorf("too many kept paths: %v", err)
	}
}

// An attached folder belongs to one agent computer: defaults would hand it to every one, including those code creates.
func TestAttachNotInDefaults(t *testing.T) {
	_, err := Parse([]byte(base + "defaults:\n  attach: { p: { host: /x/app } }\nagent_computers:\n  c:\n    harness: { kind: claude-code, version: 1.0.0 }\n    image: slim\n"))
	if err == nil || !strings.Contains(err.Error(), "defaults.attach: an attached folder belongs to one agent computer") {
		t.Fatalf("err = %v", err)
	}
}
