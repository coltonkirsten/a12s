package fleet

import "fmt"

const (
	InboxDefaultMaxMessages = 200
	InboxMinMaxMessages     = 1
	InboxCeilMaxMessages    = 2000
	InboxDefaultMaxBytes    = 320 << 10
	InboxMinMaxBytes        = 8 << 10
	InboxCeilMaxBytes       = 1 << 20
)

const (
	SourceComputer = "computer"
	SourceDefaults = "defaults"
	SourceBuiltin  = "builtin"
)

type InboxSettings struct {
	MaxMessages       int64
	MaxMessagesSource string
	MaxBytes          int64
	MaxBytesSource    string
}

// ResolveInbox reports a value equal to the default's as from defaults, since
// defaults are merged into each Computer at decode. The Fleet must have validated.
func (f *Fleet) ResolveInbox(name string) InboxSettings {
	c := f.Computers[name].Inbox
	d := f.Defaults.Inbox
	s := InboxSettings{MaxMessages: InboxDefaultMaxMessages, MaxMessagesSource: SourceBuiltin, MaxBytes: InboxDefaultMaxBytes, MaxBytesSource: SourceBuiltin}
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
	return s
}

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
	return errs
}

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
