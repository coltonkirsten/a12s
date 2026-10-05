package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// UpSummary is what `a12s up` prints (Appendix A):
//
//	created scout, builder (asleep: none)
//	updated tester
//	staged reviewer (applies when idle; a12s apply --interrupt to force)
//	removed old (drives kept)
func UpSummary(r *api.PutFleetResponse, asleep bool) []string {
	var lines []string
	if len(r.Created) > 0 {
		sleeping := "none"
		if asleep {
			sleeping = strings.Join(r.Created, ", ")
		}
		lines = append(lines, fmt.Sprintf("created %s (asleep: %s)", strings.Join(r.Created, ", "), sleeping))
	}
	if len(r.Updated) > 0 {
		lines = append(lines, "updated "+strings.Join(r.Updated, ", "))
	}
	if len(r.Staged) > 0 {
		lines = append(lines, "staged "+strings.Join(r.Staged, ", ")+" (applies when idle; a12s apply --interrupt to force)")
	}
	if len(r.Removed) > 0 {
		lines = append(lines, "removed "+strings.Join(r.Removed, ", ")+" (drives kept)")
	}
	if len(lines) == 0 {
		lines = append(lines, "no changes")
	}
	return lines
}

// ShortID abbreviates a session or turn id for a status row.
func ShortID(id string) string {
	const n = 8
	if len(id) <= n {
		return id
	}
	return id[:n] + "…"
}

// WriteStatus prints one row per Computer, then every warning. f, the local
// Fleet file, is optional; it marks Computers whose egress allows "*".
func WriteStatus(w io.Writer, st *api.StatusResponse, f *fleet.Fleet) {
	unenforced := st.EgressEnforcement == fleet.EnforcementUnenforced
	var rows [][]string
	for _, c := range st.Computers {
		cells := []string{c.Name, phase(c)}
		if c.Session != "" {
			cells = append(cells, "session "+ShortID(c.Session))
		} else {
			cells = append(cells, "session -")
		}
		h := c.Harness.Kind + " " + c.Harness.Version
		if c.Harness.Model != "" {
			h += " " + c.Harness.Model
		}
		cells = append(cells, h)
		if c.HomeSize != 0 {
			cells = append(cells, "home "+c.HomeSize.String())
		} else {
			cells = append(cells, "home -")
		}
		var extra []string
		if len(c.Drives) == 1 {
			extra = append(extra, "drive "+c.Drives[0])
		} else if len(c.Drives) > 1 {
			extra = append(extra, "drives "+strings.Join(c.Drives, ","))
		}
		if c.ActiveTurn != "" {
			extra = append(extra, "turn "+c.ActiveTurn)
		}
		if c.StagedGeneration > c.Generation {
			extra = append(extra, fmt.Sprintf("staged gen %d", c.StagedGeneration))
		}
		if spec, ok := lookup(f, c.Name); ok && spec.Egress.AllowsAll() {
			extra = append(extra, `EGRESS ALLOW "*"`)
		}
		if unenforced {
			extra = append(extra, "EGRESS UNENFORCED")
		}
		cells = append(cells, strings.Join(extra, "  "))
		rows = append(rows, cells)
	}
	writeTable(w, rows)
	for _, line := range StatusWarnings(st) {
		fmt.Fprintln(w, "warning: "+line)
	}
}

func phase(c api.ComputerStatus) string {
	p := string(c.Phase)
	if c.ResumePending {
		p += " (resumePending)"
	}
	return p
}

func lookup(f *fleet.Fleet, name string) (fleet.Computer, bool) {
	if f == nil {
		return fleet.Computer{}, false
	}
	c, ok := f.Computers[name]
	return c, ok
}

// StatusWarnings lists everything `a12s status` must say loudly: Fleet
// warnings, orphaned drives, unenforced egress, credentials not loaded, and
// each Computer's errors and warnings. Duplicates are dropped.
func StatusWarnings(st *api.StatusResponse) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if st.EgressEnforcement == fleet.EnforcementUnenforced {
		add("egress is UNENFORCED for this Fleet: allowlists are not a firewall here")
	}
	if !st.CredentialsLoaded {
		add("the Controller holds no credentials (it restarted?): run a12s up or a12s wake to send them")
	}
	for _, d := range st.OrphanedDrives {
		add(fmt.Sprintf("orphaned drive %s: no Computer in the Fleet uses it (a12s drive rm %s destroys it)", d.Name, d.Name))
	}
	for _, s := range st.Warnings {
		add(s)
	}
	for _, c := range st.Computers {
		if c.Error != nil {
			retry := "not retryable"
			if c.Error.Retryable {
				retry = "retryable"
			}
			add(fmt.Sprintf("%s: error: %s (%s)", c.Name, c.Error.Reason, retry))
		}
		for _, s := range c.Warnings {
			add(c.Name + ": " + s)
		}
	}
	return out
}

// WriteDrives prints `a12s drive ls`.
func WriteDrives(w io.Writer, drives []api.DriveRow) {
	rows := [][]string{{"NAME", "KIND", "SIZE", "ACCESS", "ATTACHED"}}
	for _, d := range drives {
		size, access, attached := "-", "-", "-"
		if d.Size != 0 {
			size = d.Size.String()
		}
		if d.Access != "" {
			access = d.Access
		}
		if len(d.AttachedTo) > 0 {
			attached = strings.Join(d.AttachedTo, ",")
		}
		if d.Orphaned {
			attached += "  ORPHANED"
		}
		rows = append(rows, []string{d.Name, string(d.Kind), size, access, attached})
	}
	writeTable(w, rows)
}

// writeTable aligns cells in columns two spaces apart, without trailing blanks.
func writeTable(w io.Writer, rows [][]string) {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		if line != "" {
			fmt.Fprintln(w, strings.TrimRight(line, " \n"))
		}
	}
}

// TurnStartedLine is `turn t_41 started (codex 0.154.0, gpt-6-astra, resumed: false)`.
// kind is the harness kind; the event carries only the version and model.
func TurnStartedLine(turn, kind string, d api.TurnStartedData) string {
	parts := []string{strings.TrimSpace(kind + " " + d.HarnessVersion)}
	if d.Model != "" {
		parts = append(parts, d.Model)
	}
	parts = append(parts, fmt.Sprintf("resumed: %t", d.Resumed))
	return fmt.Sprintf("turn %s started (%s)", turn, strings.Join(parts, ", "))
}

// TurnFinishedLine is `turn t_41 completed`, with the error when there is one.
func TurnFinishedLine(turn string, d api.TurnFinishedData) string {
	s := fmt.Sprintf("turn %s %s", turn, d.Status)
	if d.Error != "" {
		s += ": " + d.Error
	}
	return s
}

// EventLine is the one-line stderr summary of an event that is neither
// text, nor the start or end of the turn.
func EventLine(e api.Event) string {
	var m map[string]any
	_ = json.Unmarshal(e.Data, &m)
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch e.Type {
	case api.EventToolStarted, api.EventToolFinished:
		verb := "started"
		if e.Type == api.EventToolFinished {
			verb = "finished"
			if isErr, _ := m["isError"].(bool); isErr {
				verb = "failed"
			}
		}
		if name := str("name", "tool"); name != "" {
			return fmt.Sprintf("[tool] %s %s", name, verb)
		}
	case api.EventDiagnostic:
		if msg := str("message"); msg != "" {
			return "[diagnostic] " + msg
		}
	case api.EventResumeFailed:
		if r := str("reason"); r != "" {
			return "[resume.failed] " + r
		}
	}
	return fmt.Sprintf("[%s] %s", e.Type, Truncate(Compact(e.Data), 160))
}

// LogLine is one journal line of `a12s logs`: `seq type data`.
func LogLine(e api.Event) string {
	s := fmt.Sprintf("%d %s", e.Seq, e.Type)
	if d := Compact(e.Data); d != "" {
		s += " " + d
	}
	return s
}

// Compact returns JSON on one line ("" for none).
func Compact(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// Truncate shortens s to at most n runes, marking the cut.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
