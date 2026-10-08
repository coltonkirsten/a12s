package main

import (
	"testing"
	"time"
)

func TestSchedule(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	cases := []struct {
		expr, at string
		want     bool
	}{
		{"* * * * *", "2026-10-06T18:04:00Z", true},
		{"0 7 * * *", "2026-10-06T07:00:00Z", true},
		{"0 7 * * *", "2026-10-06T07:01:00Z", false},
		{"*/15 * * * *", "2026-10-06T07:45:00Z", true},
		{"*/15 * * * *", "2026-10-06T07:46:00Z", false},
		{"0 2 * * 1-5", "2026-10-05T02:00:00Z", true},  // Monday
		{"0 2 * * 1-5", "2026-10-04T02:00:00Z", false}, // Sunday
		{"0 0 1 * *", "2026-10-01T00:00:00Z", true},
		{"30 9 * * 7", "2026-10-04T09:30:00Z", true}, // 7 = Sunday
		{"5,10 * * * *", "2026-10-06T07:10:00Z", true},
	}
	for _, c := range cases {
		s, err := ParseSchedule(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Match(at(c.at)); got != c.want {
			t.Errorf("%s at %s = %v", c.expr, c.at, got)
		}
	}
	for _, bad := range []string{"* * * *", "60 * * * *", "*/0 * * * *", "a * * * *"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
