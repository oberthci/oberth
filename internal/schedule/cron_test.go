package schedule

import (
	"testing"
	"time"
)

func TestParseNumericNativeSyntax(t *testing.T) {
	for _, expr := range []string{"@daily", "0 0 0 * * *", "0 0 * JAN *", "0 0 * * MON", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8", "*/0 * * * *", "4-2 * * * *", "1,,2 * * * *", "0 0 * * -7", "0 0 * * 1/2/3", "0 0 * * 7x"} {
		t.Run(expr, func(t *testing.T) {
			if _, err := parseCron(expr, time.UTC); err == nil {
				t.Fatal("invalid syntax accepted")
			}
		})
	}
	for _, expr := range []string{"*/15 1-5 * * 0,7", "0 3 * * 5-7", "0 3 1 * *,7", "0 3 */2 * 5"} {
		if _, err := parseCron(expr, time.UTC); err != nil {
			t.Fatal(expr, err)
		}
	}
}
func TestSundayAliasPreservesNativeDayRules(t *testing.T) {
	for _, row := range []struct{ expr, want string }{
		{"0 3 * * 7", "2026-11-22T03:00:00Z"},
		{"0 3 * * 5-7", "2026-11-20T03:00:00Z"},
		{"0 3 1 * 0,7", "2026-11-22T03:00:00Z"},
		{"0 3 1 * *,7", "2026-12-01T03:00:00Z"},
		{"0 3 1 * */01,7", "2026-12-01T03:00:00Z"},
		{"0 3 */2 * 5", "2026-11-17T03:00:00Z"},
	} {
		t.Run(row.expr, func(t *testing.T) {
			s, err := parseCron(row.expr, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := time.Parse(time.RFC3339, row.want)
			if got := s.Next(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)); !got.Equal(want) {
				t.Fatal(got, want)
			}
		})
	}
}
func TestNextIsMonotonicAndSundayNormalizationIsIdempotent(t *testing.T) {
	for _, expr := range []string{"* * * * *", "1/13 0-23 * * 0-7", "0 3 29 2 *", "0 3 1 * 7"} {
		parsed, err := parseCron(expr, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		again, err := parseCron(parsed.Expression(), time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for range 20 {
			next := parsed.Next(after)
			if next.IsZero() {
				break
			}
			if !next.After(after) || !again.Next(after).Equal(next) {
				t.Fatal(expr, after, next)
			}
			after = next
		}
	}
}

func TestParseUsesProvidedTimezoneWithoutReloadingItsName(t *testing.T) {
	zone := time.FixedZone("provided-offset", 90*60)
	parsed, err := parseCron("0 3 * * *", zone)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	want := time.Date(2026, 11, 16, 1, 30, 0, 0, time.UTC)
	if got := parsed.Next(after); !got.Equal(want) {
		t.Fatal(got, want)
	}
	defaulted, err := parseCron("0 3 * * *", nil)
	if err != nil || !defaulted.Next(after).Equal(after.Add(3*time.Hour)) {
		t.Fatal(defaulted, err)
	}
}
