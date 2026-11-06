package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
)

func TestUpSummary(t *testing.T) {
	r := &api.PutFleetResponse{Created: []string{"fifth"}, Updated: []string{"scout"}, Staged: []string{"tester"}, Removed: []string{"old"}}
	got := strings.Join(UpSummary(r), "\n")
	for _, want := range []string{"fifth", "scout", "tester", "old", "drives kept", "applies when idle"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if got := UpSummary(&api.PutFleetResponse{}); len(got) != 1 || got[0] != "no changes" {
		t.Errorf("got %q", got)
	}

	// Failures are listed by name, sorted, and never summarised as "no changes".
	r = &api.PutFleetResponse{Created: []string{"scout"}, Errors: map[string]string{"tester": "image a12s/browser:dev not found", "builder": "bundle missing"}}
	if got := UpErrors(r); len(got) != 2 || !strings.HasPrefix(got[0], "builder: ") || !strings.HasPrefix(got[1], "tester: ") {
		t.Errorf("UpErrors = %q", got)
	}
	if got := UpSummary(&api.PutFleetResponse{Errors: map[string]string{"scout": "x"}}); len(got) != 0 {
		t.Errorf("summary with only errors = %q", got)
	}
}

func TestWriteStatus(t *testing.T) {
	st := &api.StatusResponse{
		Fleet: "lab",
		AgentComputers: []api.ComputerStatus{
			{Name: "scout", Phase: api.PhaseAsleep, Session: "9c41aa07-1111", Harness: api.Harness{Kind: "claude-code", Version: "2.1.280"}},
			{Name: "builder", Phase: api.PhaseBusy, Session: "2f00", ActiveTurn: "t_41", Harness: api.Harness{Kind: "codex", Version: "0.154.0"},
				Warnings: []string{"resume failed"}},
			{Name: "tester", Phase: api.PhaseError, ResumePending: true, Harness: api.Harness{Kind: "claude-code", Version: "2.1.280"}, Error: &api.Failure{Reason: "image missing", Retryable: true}, EgressMode: api.EgressAllowAll},
			{Name: "job-1", Phase: api.PhaseProvisioning, Harness: api.Harness{Kind: "claude-code", Version: "2.1.280"},
				EgressMode: api.EgressEnforced, Waiting: &api.Waiting{Credentials: []string{"dbname"}, Fix: "a12s creds set dbname"}},
		},
		OrphanedDrives: []api.DriveRow{{Name: "home-old", Kind: api.DriveHome}},
	}
	st.AgentComputers[0].EgressMode = api.EgressEnforced
	var b strings.Builder
	WriteStatus(&b, st)
	row := func(out, name string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, name+" ") {
				return l
			}
		}
		t.Fatalf("no row for %s:\n%s", name, out)
		return ""
	}
	// Only the markers and warnings an operator must see; layout is free.
	out := b.String()
	if r := row(out, "builder"); !strings.Contains(r, "t_41") {
		t.Errorf("builder row %q", r)
	}
	if r := row(out, "tester"); !strings.Contains(r, `EGRESS ALLOW "*"`) || !strings.Contains(r, "resumePending") {
		t.Errorf("tester row %q", r)
	}
	if r := row(out, "scout"); strings.Contains(r, "EGRESS") {
		t.Errorf("enforced row marked %q", r)
	}
	if r := row(out, "job-1"); !strings.Contains(r, "Provisioning (waiting for dbname)") {
		t.Errorf("job-1 row %q", r)
	}
	for _, w := range []string{"orphaned drive home-old", "builder: resume failed", "tester: error: image missing",
		"job-1: credential dbname has no value yet; it starts when you set it (a12s creds set dbname)"} {
		if !strings.Contains(out, "warning: "+w) {
			t.Errorf("no warning %q:\n%s", w, out)
		}
	}

	// A warning the Controller already gave is said once.
	st.Warnings = []string{"builder: resume failed"}
	b.Reset()
	WriteStatus(&b, st)
	out = b.String()
	if strings.Count(out, "builder: resume failed") != 1 {
		t.Errorf("warnings:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("trailing space: %q", line)
		}
	}
}

func TestBackgroundCell(t *testing.T) {
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	b := &api.Background{Tasks: 2, Items: []api.BackgroundTask{
		{ID: "b1", StartedAt: now.Add(-26 * time.Hour), Deadline: now.Add(-time.Minute)},
		{ID: "b2", StartedAt: now.Add(-30 * time.Minute), Deadline: now.Add(90 * time.Minute)},
	}}
	if got := BackgroundCell(b, now); got != "background 2 (oldest 26h, stopping)" {
		t.Errorf("got %q", got)
	}
	b.Items = b.Items[1:]
	if got := BackgroundCell(b, now); got != "background 2 (oldest 30m, next stop in 1h30m)" {
		t.Errorf("got %q", got)
	}
	if got := BackgroundCell(&api.Background{Tasks: 1}, now); got != "background 1" {
		t.Errorf("got %q", got)
	}
}
