package fleet

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	MinSleepAfterIdle      = time.Minute
	MaxSleepAfterIdle      = 7 * 24 * time.Hour
	DefaultMaxWakesPerHour = 6
	MaxMaxWakesPerHour     = 60
)

type PowerSettings struct {
	SleepAfterIdle  time.Duration // 0: off
	WakeFrom        []string      // empty: nothing wakes it
	MaxWakesPerHour int
}

func (p PowerSettings) On() bool { return p.SleepAfterIdle > 0 || len(p.WakeFrom) > 0 }

// ParseIdle accepts "off" or a duration with s, m, h or d units.
func ParseIdle(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "off" || s == "" {
		return 0, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration", s)
	}
	return d, nil
}

// ResolvePower: wakeOnMessage.from is a list, so a Computer's replaces the
// defaults' rather than merging. The Fleet must have validated.
func (f *Fleet) ResolvePower(name string) PowerSettings {
	p := f.Computers[name].Power
	out := PowerSettings{MaxWakesPerHour: DefaultMaxWakesPerHour}
	if p.SleepAfterIdle != nil {
		out.SleepAfterIdle, _ = ParseIdle(*p.SleepAfterIdle)
	}
	if p.WakeOnMessage != nil {
		out.WakeFrom = append([]string(nil), p.WakeOnMessage.From...)
		if p.WakeOnMessage.MaxWakesPerHour != nil {
			out.MaxWakesPerHour = *p.WakeOnMessage.MaxWakesPerHour
		}
	}
	return out
}

var principalNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validatePower(field string, p Power, computer func(string) bool, service func(string) bool) []error {
	var errs []error
	bad := func(sub, fix, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s.%s: %s  fix: %s", field, sub, fmt.Sprintf(format, args...), fix))
	}
	if p.SleepAfterIdle != nil {
		d, err := ParseIdle(*p.SleepAfterIdle)
		switch {
		case err != nil:
			bad("sleepAfterIdle", "use off, or 1m to 7d (e.g. 15m, 2h, 1d)", "%v", err)
		case d != 0 && (d < MinSleepAfterIdle || d > MaxSleepAfterIdle):
			bad("sleepAfterIdle", "use off, or 1m to 7d", "%s is out of range", *p.SleepAfterIdle)
		}
	}
	if w := p.WakeOnMessage; w != nil {
		if w.MaxWakesPerHour != nil && (*w.MaxWakesPerHour < 1 || *w.MaxWakesPerHour > MaxMaxWakesPerHour) {
			bad("wakeOnMessage.maxWakesPerHour", "set 1 to 60", "%d is out of range", *w.MaxWakesPerHour)
		}
		if len(w.From) == 0 {
			bad("wakeOnMessage.from", "list the principals whose messages may wake it, or remove wakeOnMessage", "is empty")
		}
		for _, pr := range w.From {
			kind, name, _ := strings.Cut(pr, ":")
			switch {
			case strings.Contains(pr, "*"):
				bad("wakeOnMessage.from", "name each principal (local, token:<name>, service:<name>, computer:<name>)", "%q: wildcards are not allowed", pr)
			case pr == "local":
			case kind == "token" && principalNameRE.MatchString(name):
			case kind == "computer" && computer(name):
			case kind == "service" && service(name):
			case kind == "computer":
				bad("wakeOnMessage.from", "add computers."+name+" or remove it", "%q: there is no Computer %s", pr, name)
			case kind == "service":
				bad("wakeOnMessage.from", "add services."+name+" or remove it", "%q: there is no Service %s", pr, name)
			default:
				bad("wakeOnMessage.from", "use local, token:<name>, service:<name> or computer:<name>", "%q is not a principal", pr)
			}
		}
	}
	return errs
}
