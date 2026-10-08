package fleet

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Inbox limits (enhancements §4.8, §4.13).
const (
	InboxDefaultMaxMessages = 200
	InboxMinMaxMessages     = 1
	InboxCeilMaxMessages    = 2000
	InboxDefaultMaxBytes    = 320 << 10
	InboxMinMaxBytes        = 8 << 10
	InboxCeilMaxBytes       = 1 << 20
	IntroMaxBytes           = 512
	IntroMaxLines           = 8
)

// DefaultIntro is the Batch intro when the Fleet sets none.
const DefaultIntro = "These messages arrived while you were busy or asleep, oldest first."

// Limit sources, as reported by the inbox API.
const (
	SourceComputer = "computer"
	SourceDefaults = "defaults"
	SourceBuiltin  = "builtin"
)

// InboxSettings are a Computer's resolved inbox settings.
type InboxSettings struct {
	MaxMessages       int64
	MaxMessagesSource string
	MaxBytes          int64
	MaxBytesSource    string
	Intro             string
}

// ResolveInbox resolves a Computer's inbox settings against the Fleet's
// defaults and the built-in values. Defaults are merged into each Computer
// at decode, so a value equal to the default's is reported as from defaults.
// The Fleet must have validated.
func (f *Fleet) ResolveInbox(name string) InboxSettings {
	c := f.Computers[name].Inbox
	d := f.Defaults.Inbox
	s := InboxSettings{MaxMessages: InboxDefaultMaxMessages, MaxMessagesSource: SourceBuiltin, MaxBytes: InboxDefaultMaxBytes, MaxBytesSource: SourceBuiltin, Intro: DefaultIntro}
	if c.MaxMessages != nil {
		s.MaxMessages, s.MaxMessagesSource = *c.MaxMessages, SourceComputer
		if d.MaxMessages != nil && *d.MaxMessages == *c.MaxMessages {
			s.MaxMessagesSource = SourceDefaults
		}
	}
	if c.MaxBytes != nil {
		if b, err := ParseBytes(*c.MaxBytes); err == nil {
			s.MaxBytes, s.MaxBytesSource = int64(b), SourceComputer
			if d.MaxBytes != nil && *d.MaxBytes == *c.MaxBytes {
				s.MaxBytesSource = SourceDefaults
			}
		}
	}
	if c.Intro != nil {
		s.Intro = *c.Intro
	}
	return s
}

// validateInbox checks one inbox block; field is its path ("defaults.inbox").
func validateInbox(field string, in Inbox) []error {
	var errs []error
	bad := func(sub, fix, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s.%s: %s  fix: %s", field, sub, fmt.Sprintf(format, args...), fix))
	}
	if in.MaxMessages != nil && (*in.MaxMessages < InboxMinMaxMessages || *in.MaxMessages > InboxCeilMaxMessages) {
		bad("maxMessages", "set 1 to 2000", "%d is out of range", *in.MaxMessages)
	}
	if in.MaxBytes != nil {
		b, err := ParseBytes(*in.MaxBytes)
		switch {
		case err != nil:
			bad("maxBytes", "use bytes or Ki/Mi, e.g. 320Ki", "%q is not a size", *in.MaxBytes)
		case b > InboxCeilMaxBytes:
			bad("maxBytes", "set 1Mi or less", "%s is above the 1Mi ceiling, which protects the model's context budget", *in.MaxBytes)
		case b < InboxMinMaxBytes:
			bad("maxBytes", "set 8Ki or more", "%s is below the 8Ki floor", *in.MaxBytes)
		}
	}
	if in.Intro != nil {
		t := *in.Intro
		switch {
		case len(t) > IntroMaxBytes:
			bad("intro", "shorten it", "%d bytes is over the 512-byte cap", len(t))
		case strings.Count(t, "\n")+1 > IntroMaxLines && t != "":
			bad("intro", "use at most 8 lines", "%d lines is over the 8-line cap", strings.Count(t, "\n")+1)
		case !utf8.ValidString(t):
			bad("intro", "use UTF-8 text", "is not valid UTF-8")
		case strings.IndexFunc(t, func(r rune) bool { return r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)) }) >= 0:
			bad("intro", "remove control characters (newlines are allowed)", "contains a control character")
		}
	}
	return errs
}

// InboxWarnings are non-fatal remarks on inbox settings, shown by `a12s up`.
func (f *Fleet) InboxWarnings() []string {
	var out []string
	for _, name := range f.ComputerNames() {
		s := f.ResolveInbox(name)
		if s.MaxBytes > InboxDefaultMaxBytes {
			out = append(out, fmt.Sprintf("%s.inbox.maxBytes %s ≈ %dk tokens per Batch; use only with a long-context model", name, Bytes(s.MaxBytes), s.MaxBytes/4/1000))
		}
	}
	return out
}
