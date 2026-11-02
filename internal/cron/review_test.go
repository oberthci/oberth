package cron

import (
	"testing"
	"time"
)

func TestVixieStepAndDaySemantics(t *testing.T) {
	for _, tc := range []struct{ expr, from, want string }{
		{"5/15 * * * *", "2026-08-24T09:05:00Z", "2026-08-24T09:20:00Z"},
		{"0 0 */2 * 1", "2026-08-24T00:00:00Z", "2026-08-31T00:00:00Z"},
		{"0 0 1 * */2", "2026-08-24T00:00:00Z", "2026-09-01T00:00:00Z"},
		{"*/5 * * * *", "2026-08-24T09:05:00Z", "2026-08-24T09:10:00Z"},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			s, err := Parse(tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Next(at(t, tc.from))
			if err != nil || !got.Equal(at(t, tc.want)) {
				t.Fatalf("Next = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestIntervalFindsMonthBoundaryAndLeapGaps(t *testing.T) {
	for _, tc := range []struct {
		expr string
		gap  time.Duration
	}{
		{"0,59 0,23 1,31 * *", time.Minute},
		{"0 0 29 2 *", 4*365*24*time.Hour + 24*time.Hour},
		{"0 0 30 2 *", 0},
	} {
		s, err := Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Interval(at(t, "2025-12-31T23:59:00Z"))
		if tc.gap == 0 {
			if err == nil {
				t.Fatal("impossible date has interval")
			}
			continue
		}
		if err != nil || got != tc.gap {
			t.Errorf("%s: Interval = %s, %v; want %s", tc.expr, got, err, tc.gap)
		}
	}
}
