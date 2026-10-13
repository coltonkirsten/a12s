package cli

import (
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

func TestUpSummary(t *testing.T) {
	r := &api.PutFleetResponse{Created: []string{"fifth"}, Updated: []string{"scout"}, Staged: []string{"tester"}, Removed: []string{"old"}}
	got := strings.Join(UpSummary(r), "\n")
	for _, want := range []string{"fifth", "scout", "tester", "old", "drives kept", "apply --interrupt"} {
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
	gi := fleet.Bytes(10 << 30)
	st := &api.StatusResponse{
		Fleet:             "lab",
		EgressEnforcement: "enforced",
		AgentComputers: []api.ComputerStatus{
			{Name: "scout", Phase: api.PhaseAsleep, Session: "9c41aa07-1111", Harness: api.Harness{Kind: "claude-code", Version: "2.1.280"}, HomeSize: gi},
			{Name: "builder", Phase: api.PhaseBusy, Session: "2f00", ActiveTurn: "t_41", Harness: api.Harness{Kind: "codex", Version: "0.154.0"}, HomeSize: gi, Drives: []string{"a12s-repo"},
				Warnings: []string{"resume failed"}},
			{Name: "tester", Phase: api.PhaseError, ResumePending: true, Harness: api.Harness{Kind: "claude-code", Version: "2.1.280"}, Error: &api.Failure{Reason: "image missing", Retryable: true}, EgressMode: api.EgressAllowAll},
			{Name: "job-1", Phase: api.PhaseProvisioning, Harness: api.Harness{Kind: "claude-code", Version: "2.1.280"},
				EgressMode: api.EgressEnforced, Waiting: &api.Waiting{Credentials: []string{"dbname"}, Fix: "a12s creds set dbname"}},
		},
		OrphanedDrives: []api.DriveRow{{Name: "ux-old", Kind: api.DriveWork}},
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
	if r := row(out, "builder"); !strings.Contains(r, "t_41") || !strings.Contains(r, "a12s-repo") {
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
	for _, w := range []string{"orphaned drive ux-old", "builder: resume failed", "tester: error: image missing",
		"job-1: credential dbname has no value yet; it starts when you set it (a12s creds set dbname)"} {
		if !strings.Contains(out, "warning: "+w) {
			t.Errorf("no warning %q:\n%s", w, out)
		}
	}

	// Unenforced egress is said on every row and once loudly.
	// From the per-row mode; a Controller without it falls back to the
	// Fleet-wide enforcement for every row.
	st.EgressEnforcement = "unenforced"
	st.Warnings = []string{"egress is UNENFORCED for this Fleet: allowlists are not a firewall here"} // duplicate dropped
	st.AgentComputers[0].EgressMode = api.EgressUnenforced
	b.Reset()
	WriteStatus(&b, st)
	out = b.String()
	if strings.Count(out, "EGRESS UNENFORCED") != 2 || strings.Count(out, `EGRESS ALLOW "*"`) != 1 {
		t.Errorf("egress markers:\n%s", out)
	}
	if strings.Count(out, "egress is UNENFORCED") != 1 {
		t.Errorf("warnings:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("trailing space: %q", line)
		}
	}
}
