package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a five-field cron expression: minute hour day-of-month month
// day-of-week, each "*", "*/n", "a", "a-b", "a-b/n" or a comma list.
type Schedule struct {
	min, hour, dom, mon, dow [64]bool
	domStar, dowStar         bool
}

func ParseSchedule(expr string) (*Schedule, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day month weekday)", expr)
	}
	s := &Schedule{domStar: f[2] == "*", dowStar: f[4] == "*"}
	for i, spec := range []struct {
		set      *[64]bool
		lo, hi   int
		fieldStr string
	}{{&s.min, 0, 59, f[0]}, {&s.hour, 0, 23, f[1]}, {&s.dom, 1, 31, f[2]}, {&s.mon, 1, 12, f[3]}, {&s.dow, 0, 7, f[4]}} {
		if err := parseField(spec.fieldStr, spec.lo, spec.hi, spec.set); err != nil {
			return nil, fmt.Errorf("cron %q field %d: %w", expr, i+1, err)
		}
	}
	if s.dow[7] {
		s.dow[0] = true
	}
	return s, nil
}

func parseField(f string, lo, hi int, set *[64]bool) error {
	for _, part := range strings.Split(f, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepS)
			if err != nil || n < 1 {
				return fmt.Errorf("bad step %q", stepS)
			}
			step = n
		}
		a, b := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			as, bs, _ := strings.Cut(rng, "-")
			var err1, err2 error
			a, err1 = strconv.Atoi(as)
			b, err2 = strconv.Atoi(bs)
			if err1 != nil || err2 != nil {
				return fmt.Errorf("bad range %q", rng)
			}
		default:
			n, err := strconv.Atoi(rng)
			if err != nil {
				return fmt.Errorf("bad value %q", rng)
			}
			a, b = n, n
			if hasStep {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return fmt.Errorf("%q is out of range %d-%d", part, lo, hi)
		}
		for v := a; v <= b; v += step {
			set[v] = true
		}
	}
	return nil
}

// Match reports whether t (truncated to the minute) is a slot.
func (s *Schedule) Match(t time.Time) bool {
	if !s.min[t.Minute()] || !s.hour[t.Hour()] || !s.mon[int(t.Month())] {
		return false
	}
	dom, dow := s.dom[t.Day()], s.dow[int(t.Weekday())]
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	}
	return dom || dow
}
