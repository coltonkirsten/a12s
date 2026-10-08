package main

import (
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/fleet"
)

func TestScaffoldParses(t *testing.T) {
	for _, h := range []string{"claude-code", "codex"} {
		for _, with := range [][]string{nil, {"db"}, {"cron"}, {"db", "cron"}} {
			text, _, err := scaffold("My_Project", h, with)
			if err != nil {
				t.Fatal(err)
			}
			f, err := fleet.Parse([]byte(text))
			if err != nil {
				t.Fatalf("%s %v:\n%s\n%v", h, with, text, err)
			}
			if f.Metadata.Name != "fleet" || len(f.Services) != 2*boolInt(contains(with, "db"))+boolInt(contains(with, "cron")) {
				t.Fatalf("%s %v: %+v", h, with, f.Services)
			}
		}
	}
	text, _, _ := scaffold("lab", "claude-code", []string{"db"})
	if !strings.Contains(text, "# uses: [db]") {
		t.Fatal("db scaffold lacks the commented uses line")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
