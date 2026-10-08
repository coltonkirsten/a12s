package fleet

import (
	"strings"
	"testing"
)

func p[T any](v T) *T { return &v }

// validateWith loads the lab Fleet, edits it and validates it.
func validateWith(t *testing.T, edit func(f *Fleet)) (*Fleet, error) {
	t.Helper()
	f, err := Load("testdata/lab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	edit(f)
	return f, f.Validate()
}

// refuses checks an error names the field and the offending value and
// carries a fix (§6.3); the prose is not this test's business.
func refuses(t *testing.T, err error, field, value string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), field+": ") || !strings.Contains(err.Error(), value) || !strings.Contains(err.Error(), "fix:") {
		t.Errorf("want %s refused (%s): %v", field, value, err)
	}
}

// Moved from the controller package (test pass #18): the inbox block's
// bounds are the schema contract.
func TestInboxValidation(t *testing.T) {
	const field = "computers.builder.inbox."
	for _, c := range []struct {
		in         Inbox
		sub, value string
	}{
		{Inbox{MaxBytes: p("2Mi")}, "maxBytes", "2Mi"},
		{Inbox{MaxBytes: p("4Ki")}, "maxBytes", "4Ki"},
		{Inbox{MaxBytes: p("lots")}, "maxBytes", "lots"},
		{Inbox{MaxMessages: p(int64(0))}, "maxMessages", "0"},
		{Inbox{MaxMessages: p(int64(2001))}, "maxMessages", "2001"},
		{Inbox{Intro: p(strings.Repeat("x", 513))}, "intro", "513 bytes"},
		{Inbox{Intro: p(strings.Repeat("l\n", 8) + "l")}, "intro", "9 lines"},
		{Inbox{Intro: p("bell\a")}, "intro", "control"},
	} {
		_, err := validateWith(t, func(f *Fleet) {
			b := f.Computers["builder"]
			b.Inbox = c.in
			f.Computers["builder"] = b
		})
		refuses(t, err, field+c.sub, c.value)
	}
	ok, err := validateWith(t, func(f *Fleet) {
		b := f.Computers["builder"]
		b.Inbox = Inbox{MaxBytes: p("768Ki"), Intro: p("Review requests.\nOne verdict each.")}
		f.Computers["builder"] = b
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := ok.InboxWarnings(); len(w) != 1 || !strings.HasPrefix(w[0], "builder.inbox.maxBytes") {
		t.Fatalf("warnings %v", w)
	}
}

// Moved from the controller package (test pass #18): power policy bounds
// and wake principals that must exist.
func TestPowerValidation(t *testing.T) {
	const field = "computers.scout.power."
	for _, c := range []struct {
		p          Power
		sub, value string
	}{
		{Power{WakeOnMessage: &WakeOnMessage{From: []string{"*"}}}, "wakeOnMessage.from", `"*"`},
		{Power{WakeOnMessage: &WakeOnMessage{From: []string{"local"}, MaxWakesPerHour: p(0)}}, "wakeOnMessage.maxWakesPerHour", "0"},
		{Power{WakeOnMessage: &WakeOnMessage{From: []string{"local"}, MaxWakesPerHour: p(61)}}, "wakeOnMessage.maxWakesPerHour", "61"},
		{Power{WakeOnMessage: &WakeOnMessage{From: []string{"computer:nobody"}}}, "wakeOnMessage.from", "no Computer nobody"},
		{Power{WakeOnMessage: &WakeOnMessage{From: []string{"service:clock"}}}, "wakeOnMessage.from", "no Service clock"},
		{Power{SleepAfterIdle: p("30s")}, "sleepAfterIdle", "30s"},
		{Power{SleepAfterIdle: p("8d")}, "sleepAfterIdle", "8d"},
		{Power{SleepAfterIdle: p("soon")}, "sleepAfterIdle", "soon"},
	} {
		_, err := validateWith(t, func(f *Fleet) {
			s := f.Computers["scout"]
			s.Power = c.p
			f.Computers["scout"] = s
		})
		refuses(t, err, field+c.sub, c.value)
	}
}
