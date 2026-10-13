package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/coltonkirsten/a12s/internal/api"
	"github.com/coltonkirsten/a12s/internal/fleet"
)

// UpSummary prints:
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
	if len(r.Services.Created) > 0 {
		lines = append(lines, "created service "+strings.Join(r.Services.Created, ", "))
	}
	if len(r.Services.Updated) > 0 {
		lines = append(lines, "restarted service "+strings.Join(r.Services.Updated, ", ")+" (new generation)")
	}
	if len(r.Services.Removed) > 0 {
		lines = append(lines, "removed service "+strings.Join(r.Services.Removed, ", ")+" (state drives kept)")
	}
	for _, w := range r.Warnings {
		lines = append(lines, "warning: "+w)
	}
	if len(lines) == 0 && len(r.Errors) == 0 {
		lines = append(lines, "no changes")
	}
	return lines
}

// SeccompLines names every Computer that runs with its own seccomp profile instead of the default.
func SeccompLines(f *fleet.Fleet) []string {
	var out []string
	for _, name := range f.ComputerNames() {
		if p := f.Computers[name].Security.Seccomp; !p.IsZero() {
			out = append(out, fmt.Sprintf("%s runs with its own seccomp profile", name))
		}
	}
	return out
}

func UpErrors(r *api.PutFleetResponse) []string {
	var out []string
	for _, name := range sortedKeys(r.Errors) {
		out = append(out, name+": "+r.Errors[name])
	}
	return out
}

func ShortID(id string) string {
	const n = 8
	if len(id) <= n {
		return id
	}
	return id[:n] + "…"
}

// WriteStatus takes egress markers from the Controller's observed EgressMode,
// never the local Fleet file, which may not be applied.
func WriteStatus(w io.Writer, st *api.StatusResponse) {
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
		if m := egressMarker(st, c); m != "" {
			extra = append(extra, m)
		}
		if c.Seccomp != "" {
			extra = append(extra, "seccomp "+c.Seccomp)
		}
		if in := InboxCell(c.Inbox); in != "" {
			extra = append(extra, in)
		}
		if pw := PowerCell(c.Power, c.Phase); pw != "" {
			extra = append(extra, "power: "+pw)
		}
		cells = append(cells, strings.Join(extra, "  "))
		rows = append(rows, cells)
	}
	writeTable(w, rows)
	if len(st.Services) > 0 {
		fmt.Fprintln(w)
		WriteServices(w, st.Services)
	}
	for _, line := range StatusWarnings(st) {
		fmt.Fprintln(w, "warning: "+line)
	}
}

func WriteServices(w io.Writer, services []api.ServiceStatus) {
	rows := [][]string{{"SERVICE", "PHASE", "IMAGE", "PIN", "RESTARTS", "SEND", "USED BY", "PUBLISH"}}
	dash := func(xs []string) string {
		if len(xs) == 0 {
			return "-"
		}
		return strings.Join(xs, ",")
	}
	for _, s := range services {
		pin := "-"
		if p := ShortImage(s.Pin); p != "" {
			pin = p
		}
		pub := "-"
		if len(s.Publish) > 0 {
			pub = "PUBLISHED " + strings.Join(s.Publish, ",") + " (reachable outside uses)"
		}
		ph := string(s.Phase)
		if s.EgressMode == api.EgressAllowAll {
			ph += `  EGRESS ALLOW "*"`
		}
		rows = append(rows, []string{s.Name, ph, s.Image, pin, fmt.Sprint(s.Restarts), dash(s.Send), dash(s.UsedBy), pub})
	}
	writeTable(w, rows)
}

func ShortImage(id string) string {
	h := strings.TrimPrefix(id, "sha256:")
	if len(h) < 12 {
		return ""
	}
	return h[:12]
}

func phase(c api.ComputerStatus) string {
	p := string(c.Phase)
	if c.Phase == api.PhaseAsleep && c.Power != nil && c.Power.SleptBy != "" {
		switch c.Power.SleptBy {
		case api.SleptAuto:
			at := ""
			if c.Power.SleptAt != nil {
				at = " " + c.Power.SleptAt.Local().Format("15:04")
			}
			p += " (auto" + at + ")"
		case api.SleptManual:
			p += " (manual)"
		}
	}
	if c.ResumePending {
		p += " (resumePending)"
	}
	return p
}

func egressMarker(st *api.StatusResponse, c api.ComputerStatus) string {
	mode := c.EgressMode
	if mode == "" && st.EgressEnforcement == fleet.EnforcementUnenforced {
		mode = api.EgressUnenforced
	}
	switch mode {
	case api.EgressAllowAll:
		return `EGRESS ALLOW "*"`
	case api.EgressUnenforced:
		return "EGRESS UNENFORCED"
	}
	return ""
}

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
	for _, sv := range st.Services {
		if sv.Error != nil {
			add(fmt.Sprintf("%s: error: %s", sv.Name, sv.Error.Reason))
		}
		for _, s := range sv.Warnings {
			add(sv.Name + ": " + s)
		}
	}
	return out
}

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
		if d.Kind == api.DriveState && access == "-" {
			access = "exclusive"
		}
		rows = append(rows, []string{d.Name, string(d.Kind), size, access, attached})
	}
	writeTable(w, rows)
}

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

// TurnStartedLine uses kind only when the event does not name the harness.
func TurnStartedLine(turn, kind string, d api.TurnStartedData) string {
	if d.Harness.Kind != "" {
		kind = d.Harness.Kind
	}
	parts := []string{strings.TrimSpace(kind + " " + d.Harness.Version)}
	if d.Model != "" {
		parts = append(parts, d.Model)
	}
	parts = append(parts, fmt.Sprintf("resumed: %t", d.Resumed))
	if d.Inbox != nil && len(d.Inbox.Messages) > 1 {
		parts = append(parts, fmt.Sprintf("batch of %d", len(d.Inbox.Messages)))
	}
	return fmt.Sprintf("turn %s started (%s)", turn, strings.Join(parts, ", "))
}

func BatchStartedLine(turn, kind, mine string, d api.TurnStartedData) string {
	line := TurnStartedLine(turn, kind, d)
	if d.Inbox == nil || len(d.Inbox.Messages) < 2 {
		return line
	}
	var others []string
	seen := map[string]bool{}
	for _, m := range d.Inbox.Messages {
		if m.ID != mine && !seen[m.Sender] {
			seen[m.Sender] = true
			others = append(others, m.Sender)
		}
	}
	return strings.Replace(line, fmt.Sprintf("batch of %d", len(d.Inbox.Messages)),
		fmt.Sprintf("batch of %d: yours + %s", len(d.Inbox.Messages), strings.Join(others, ", ")), 1)
}

func TurnFinishedLine(turn string, d api.TurnFinishedData) string {
	s := fmt.Sprintf("turn %s %s", turn, d.Status)
	if d.Error != "" {
		s += ": " + d.Error
	}
	return s
}

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

func LogLine(e api.Event) string {
	s := fmt.Sprintf("%d %s", e.Seq, e.Type)
	if d := Compact(e.Data); d != "" {
		s += " " + d
	}
	return s
}

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

func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func InboxCell(in *api.InboxSummary) string {
	if in == nil || (in.Usage.Messages == 0 && !in.Held) {
		return ""
	}
	s := fmt.Sprintf("inbox %d/%d · %s", in.Usage.Messages, in.Limits.MaxMessages.Value, kib(in.Usage.Bytes))
	if in.OverLimit {
		s += " OVER (refusing)"
	}
	if in.Held {
		s += " HELD (" + in.HoldReason + ")"
	}
	return s
}

func kib(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%dKi", (n+1023)/1024)
}

func WriteInbox(w io.Writer, l *api.InboxListResponse, bodies bool) {
	held := "not held"
	if l.Held {
		held = "HELD (" + l.HoldReason + ")"
		if l.Diagnostic != "" {
			held += ": " + l.Diagnostic
		}
	}
	orphan := ""
	if l.Orphaned {
		orphan = " · ORPHANED (no longer in the Fleet)"
	}
	fmt.Fprintf(w, "%s inbox · %d pending, %d claimed · %s%s\n", l.Computer, l.Pending, l.Claimed, held, orphan)
	over := ""
	if l.OverLimit {
		over = " · OVER LIMIT: new messages are refused until it drains"
	}
	fmt.Fprintf(w, "limits %d messages (%s), %s (%s) · used %d, %s%s\n", l.Limits.MaxMessages.Value, l.Limits.MaxMessages.Source,
		kib(l.Limits.MaxBytes.Value), l.Limits.MaxBytes.Source, l.Usage.Messages, kib(l.Usage.Bytes), over)
	e := l.Ended24h
	fmt.Fprintf(w, "last 24h: %d expired, %d replaced, %d withdrawn, %d discarded\n", e.Expired, e.Replaced, e.Withdrawn, e.Discarded)
	if l.SessionOverride != "" {
		fmt.Fprintf(w, "next Batch starts a %s session (inbox resume --new)\n", l.SessionOverride)
	}
	if len(l.Messages) == 0 {
		return
	}
	rows := [][]string{{"SEQ", "ID", "FROM", "VIA", "AT", "STATE", "EXPIRES", "KEY"}}
	for _, m := range l.Messages {
		exp := "-"
		if m.ExpiresAt != nil {
			exp = m.ExpiresAt.Local().Format("15:04:05")
		}
		key := "-"
		if m.ReplaceKey != "" {
			key = fmt.Sprintf("%s (%d)", m.ReplaceKey, m.Superseded)
		}
		state := m.State
		if m.Fresh {
			state += " fresh"
		}
		rows = append(rows, []string{fmt.Sprint(m.Seq), m.ID, m.Sender, m.Via, m.CreatedAt.Local().Format("15:04:05"), state, exp, key})
	}
	writeTable(w, rows)
	if bodies {
		for _, m := range l.Messages {
			if m.Body != nil {
				fmt.Fprintf(w, "\n--- %s (seq %d, from %s via %s)\n%s\n", m.ID, m.Seq, m.Sender, m.Via, *m.Body)
			}
		}
	}
}

func PowerCell(ps *api.PowerStatus, phase api.Phase) string {
	if ps == nil {
		return ""
	}
	var parts []string
	switch ps.AutoWake {
	case api.AutoWakeSuppressed:
		return "auto-wake suppressed until a12s wake"
	case api.AutoWakePaused:
		s := "auto-wake: paused (" + strings.ReplaceAll(ps.Reason, "_", " ") + ")"
		if ps.Fix != "" {
			s += "  fix: " + ps.Fix
		}
		return s
	case api.AutoWakeRateLimited:
		s := fmt.Sprintf("auto-wake: rate-limited (%d/%d)", ps.WakesLastHour, ps.MaxWakesPerHour)
		if ps.NextWakeAt != nil {
			s += ", next " + ps.NextWakeAt.Local().Format("15:04")
		}
		return s
	case api.AutoWakeOn:
		parts = append(parts, fmt.Sprintf("wakes on %s · %d/%d this hour", strings.Join(ps.WakeOnMessage, ", "), ps.WakesLastHour, ps.MaxWakesPerHour))
	}
	if ps.SleepAfterIdle != "off" && ps.SleepAfterIdle != "" && phase != api.PhaseAsleep {
		if ps.IdleSince != nil {
			parts = append(parts, fmt.Sprintf("idle %s of %s", time.Since(*ps.IdleSince).Round(time.Minute), ps.SleepAfterIdle))
		} else {
			parts = append(parts, "sleeps after "+ps.SleepAfterIdle+" idle")
		}
	}
	return strings.Join(parts, " · ")
}

func WriteServiceDetail(w io.Writer, s api.ServiceStatus) {
	dash := func(xs []string) string {
		if len(xs) == 0 {
			return "-"
		}
		return strings.Join(xs, ", ")
	}
	rows := [][]string{
		{"service", s.Name},
		{"phase", string(s.Phase)},
		{"image", s.Image},
		{"pin", s.Pin},
		{"generation", fmt.Sprint(s.Generation)},
		{"restarts", fmt.Sprint(s.Restarts)},
		{"health", s.Health + sinceSuffix(s.HealthSince)},
		{"ports", dash(s.Ports)},
		{"send", dash(s.Send)},
		{"uses", dash(s.Uses)},
		{"used by", dash(s.UsedBy)},
		{"publish", dash(s.Publish)},
		{"state drive", s.StateDrive},
	}
	if s.Error != nil {
		rows = append(rows, []string{"error", s.Error.Reason})
	}
	writeTable(w, rows)
	for _, x := range s.Warnings {
		fmt.Fprintln(w, "warning: "+x)
	}
}

func sinceSuffix(t string) string {
	if t == "" {
		return ""
	}
	return " (since " + t + ")"
}
