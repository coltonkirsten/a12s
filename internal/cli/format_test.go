package cli

import (
	"strings"
	"testing"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

func TestUpSummary(t *testing.T) {
	r := &api.PutFleetResponse{Created: []string{"scout", "builder", "tester", "reviewer"}}
	if got := UpSummary(r, false); strings.Join(got, "\n") != "created scout, builder, tester, reviewer (asleep: none)" {
		t.Errorf("got %q", got)
	}
	r = &api.PutFleetResponse{Created: []string{"fifth"}, Updated: []string{"scout"}, Staged: []string{"tester"}, Removed: []string{"old"}}
	want := []string{
		"created fifth (asleep: fifth)",
		"updated scout",
		"staged tester (applies when idle; a12s apply --interrupt to force)",
		"removed old (drives kept)",
	}
	if got := UpSummary(r, true); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %q", got)
	}
	if got := UpSummary(&api.PutFleetResponse{}, false); got[0] != "no changes" {
		t.Errorf("got %q", got)
	}

	// Failures are listed by name, and never summarised as "no changes".
	r = &api.PutFleetResponse{Created: []string{"scout"}, Errors: map[string]string{"tester": "image a12s/browser:dev not found", "builder": "bundle missing"}}
	if got := UpErrors(r); strings.Join(got, "\n") != "builder: bundle missing\ntester: image a12s/browser:dev not found" {
		t.Errorf("UpErrors = %q", got)
	}
	if got := UpSummary(&api.PutFleetResponse{Errors: map[string]string{"scout": "x"}}, false); len(got) != 0 {
		t.Errorf("summary with only errors = %q", got)
	}
}

func TestWriteStatus(t *testing.T) {
	gi := fleet.Bytes(10 << 30)
	st := &api.StatusResponse{
		Fleet:             "lab",
		EgressEnforcement: "enforced",
		CredentialsLoaded: true,
		Computers: []api.ComputerStatus{
			{Name: "scout", Phase: api.PhaseAsleep, Session: "9c41aa07-1111", Harness: api.Harness{Kind: "claude-code", Version: "2.1.280"}, HomeSize: gi},
			{Name: "builder", Phase: api.PhaseBusy, Session: "2f00", ActiveTurn: "t_41", Harness: api.Harness{Kind: "codex", Version: "0.154.0"}, HomeSize: gi, Drives: []string{"a12s-repo"},
				Warnings: []string{"resume failed"}},
			{Name: "tester", Phase: api.PhaseError, ResumePending: true, Harness: api.Harness{Kind: "claude-code", Version: "2.1.280"}, Error: &api.Failure{Reason: "image missing", Retryable: true}, EgressMode: api.EgressAllowAll},
		},
		OrphanedDrives: []api.DriveRow{{Name: "ux-old", Kind: api.DriveWork}},
	}
	st.Computers[0].EgressMode = api.EgressEnforced
	var b strings.Builder
	WriteStatus(&b, st)
	want := `scout    Asleep                 session 9c41aa07…  claude-code 2.1.280  home 10Gi
builder  Busy                   session 2f00       codex 0.154.0        home 10Gi  drive a12s-repo  turn t_41
tester   Error (resumePending)  session -          claude-code 2.1.280  home -     EGRESS ALLOW "*"
warning: orphaned drive ux-old: no Computer in the Fleet uses it (a12s drive rm ux-old destroys it)
warning: builder: resume failed
warning: tester: error: image missing (retryable)
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}

	// Unenforced egress and lost credentials are said on every row / loudly.
	// From the per-row mode; a Controller without it falls back to the
	// Fleet-wide enforcement for every row.
	st.EgressEnforcement = "unenforced"
	st.CredentialsLoaded = false
	st.Warnings = []string{"egress is UNENFORCED for this Fleet: allowlists are not a firewall here"} // duplicate dropped
	st.Computers[0].EgressMode = api.EgressUnenforced
	b.Reset()
	WriteStatus(&b, st)
	out := b.String()
	if strings.Count(out, "EGRESS UNENFORCED") != 2 || strings.Count(out, `EGRESS ALLOW "*"`) != 1 {
		t.Errorf("egress markers:\n%s", out)
	}
	if strings.Count(out, "egress is UNENFORCED") != 1 || !strings.Contains(out, "holds no credentials") {
		t.Errorf("warnings:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("trailing space: %q", line)
		}
	}
}
