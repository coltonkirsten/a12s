package main

import (
	"testing"

	"github.com/coltonkirsten/a12s/internal/fleet"
)

func TestScaffoldParses(t *testing.T) {
	for _, h := range []string{"claude-code", "codex"} {
		text, _, err := scaffold("My_Project", h)
		if err != nil {
			t.Fatal(err)
		}
		f, err := fleet.Parse([]byte(text))
		if err != nil {
			t.Fatalf("%s:\n%s\n%v", h, text, err)
		}
		if f.Metadata.Name != "fleet" || len(f.Services) != 0 || f.Computers["assistant"].Image != fleet.TierSlim {
			t.Fatalf("%s: %+v", h, f)
		}
	}
}
