package cli

import (
	"encoding/json"
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
			{Name: "tester", Phase: api.PhaseError, ResumePending: true, Harness: api.Harness{Kind: "claude-code", Version: "2.1.280"}, Error: &api.Failure{Reason: "image missing", Retryable: true}},
		},
		OrphanedDrives: []api.DriveRow{{Name: "ux-old", Kind: api.DriveWork}},
	}
	f := &fleet.Fleet{Computers: map[string]fleet.Computer{"tester": {Egress: fleet.Egress{Allow: []string{"*"}}}}}
	var b strings.Builder
	WriteStatus(&b, st, f)
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
	st.EgressEnforcement = "unenforced"
	st.CredentialsLoaded = false
	st.Warnings = []string{"egress is UNENFORCED for this Fleet: allowlists are not a firewall here"} // duplicate dropped
	b.Reset()
	WriteStatus(&b, st, nil)
	out := b.String()
	if strings.Count(out, "EGRESS UNENFORCED") != 3 {
		t.Errorf("unenforced not on every row:\n%s", out)
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

func TestWriteDrives(t *testing.T) {
	var b strings.Builder
	WriteDrives(&b, []api.DriveRow{
		{Name: "home-scout", Kind: api.DriveHome, Computer: "scout", Size: 10 << 30, AttachedTo: []string{"scout"}},
		{Name: "reference", Kind: api.DriveWork, Size: 5 << 30, Access: "shared", AttachedTo: []string{"reviewer", "scout"}},
		{Name: "ux-old", Kind: api.DriveWork, Access: "exclusive", Orphaned: true},
	})
	want := `NAME        KIND  SIZE  ACCESS     ATTACHED
home-scout  home  10Gi  -          scout
reference   work  5Gi   shared     reviewer,scout
ux-old      work  -     exclusive  -  ORPHANED
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestTurnLines(t *testing.T) {
	got := TurnStartedLine("t_41", "codex", api.TurnStartedData{Harness: api.Harness{Kind: "codex", Version: "0.154.0"}, Model: "gpt-6-astra"})
	if got != "turn t_41 started (codex 0.154.0, gpt-6-astra, resumed: false)" {
		t.Errorf("got %q", got)
	}
	got = TurnStartedLine("t_2", "claude-code", api.TurnStartedData{Harness: api.Harness{Version: "2.1.280"}, Resumed: true})
	if got != "turn t_2 started (claude-code 2.1.280, resumed: true)" {
		t.Errorf("got %q", got)
	}
	if got := TurnFinishedLine("t_41", api.TurnFinishedData{Status: "completed"}); got != "turn t_41 completed" {
		t.Errorf("got %q", got)
	}
	if got := TurnFinishedLine("t_9", api.TurnFinishedData{Status: "failed", Error: "resume failed"}); got != "turn t_9 failed: resume failed" {
		t.Errorf("got %q", got)
	}
}

func TestEventAndLogLines(t *testing.T) {
	ev := func(typ api.EventType, data string) api.Event {
		return api.Event{Seq: 12, Type: typ, Data: json.RawMessage(data)}
	}
	for _, c := range []struct {
		e    api.Event
		want string
	}{
		{ev(api.EventToolStarted, `{"name":"Bash","input":{"command":"ls"}}`), "[tool] Bash started"},
		{ev(api.EventToolFinished, `{"name":"Bash","isError":true}`), "[tool] Bash failed"},
		{ev(api.EventDiagnostic, `{"message":"egress denied: evil.com:443"}`), "[diagnostic] egress denied: evil.com:443"},
		{ev(api.EventPermission, `{"tool": "Write", "decision": "deny"}`), `[permission] {"tool":"Write","decision":"deny"}`},
		{ev(api.EventResumeFailed, `{"sessionId":"s","reason":"no session file","at":"wake"}`), "[resume.failed] no session file"},
		{ev(api.EventLifecycle, ``), "[lifecycle] "},
	} {
		if got := EventLine(c.e); got != c.want {
			t.Errorf("EventLine(%s) = %q, want %q", c.e.Type, got, c.want)
		}
	}
	if got := LogLine(ev(api.EventTextDelta, `{ "text": "hi" }`)); got != `12 text.delta {"text":"hi"}` {
		t.Errorf("LogLine = %q", got)
	}
	if got := LogLine(ev(api.EventLifecycle, ``)); got != "12 lifecycle" {
		t.Errorf("LogLine = %q", got)
	}
	if got := Truncate("abcdef", 4); got != "abc…" {
		t.Errorf("Truncate = %q", got)
	}
}
