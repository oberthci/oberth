package cron

import (
	"strings"
	"testing"
	"time"
)

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.UTC()
}

func TestNextFiresAtTheExpectedInstant(t *testing.T) {
	t.Parallel()
	from := "2026-08-24T09:15:30Z"
	cases := map[string]struct {
		expression string
		want       string
	}{
		"every minute":                   {"* * * * *", "2026-08-24T09:16:00Z"},
		"top of the next hour":           {"0 * * * *", "2026-08-24T10:00:00Z"},
		"nightly at three":               {"0 3 * * *", "2026-08-25T03:00:00Z"},
		"every five minutes":             {"*/5 * * * *", "2026-08-24T09:20:00Z"},
		"a minute list":                  {"7,23,41 * * * *", "2026-08-24T09:23:00Z"},
		"a minute range":                 {"20-25 * * * *", "2026-08-24T09:20:00Z"},
		"a stepped range":                {"0-30/10 * * * *", "2026-08-24T09:20:00Z"},
		"weekly on monday":               {"0 4 * * 1", "2026-08-31T04:00:00Z"},
		"weekly on sunday as zero":       {"0 4 * * 0", "2026-08-30T04:00:00Z"},
		"weekly on sunday as seven":      {"0 4 * * 7", "2026-08-30T04:00:00Z"},
		"first of the month":             {"0 0 1 * *", "2026-09-01T00:00:00Z"},
		"a specific month":               {"0 0 1 1 *", "2027-01-01T00:00:00Z"},
		"later the same hour":            {"30 9 * * *", "2026-08-24T09:30:00Z"},
		"this minute does not count":     {"15 9 * * *", "2026-08-25T09:15:00Z"},
		"an hour list crossing midnight": {"0 2,22 * * *", "2026-08-24T22:00:00Z"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			schedule, err := Parse(tc.expression)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.expression, err)
			}
			got, err := schedule.Next(at(t, from))
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if !got.Equal(at(t, tc.want)) {
				t.Fatalf("Next(%q) = %s, want %s", tc.expression, got.Format(time.RFC3339), tc.want)
			}
		})
	}
}

func TestNextIsStrictlyAfterTheGivenInstant(t *testing.T) {
	t.Parallel()
	schedule, err := Parse("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	exactly := at(t, "2026-08-24T03:00:00Z")
	got, err := schedule.Next(exactly)
	if err != nil {
		t.Fatal(err)
	}
	if !got.After(exactly) {
		t.Fatalf("Next returned %s, which is not after %s; a fire would repeat forever",
			got.Format(time.RFC3339), exactly.Format(time.RFC3339))
	}
}

func TestDayOfMonthAndDayOfWeekAreOredWhenBothAreRestricted(t *testing.T) {
	t.Parallel()
	schedule, err := Parse("0 0 1 * 5")
	if err != nil {
		t.Fatal(err)
	}
	got, err := schedule.Next(at(t, "2026-08-24T09:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if want := at(t, "2026-08-28T00:00:00Z"); !got.Equal(want) {
		t.Fatalf("Next = %s, want the next Friday %s; AND semantics would give 2026-09-01 or later",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestDayOfMonthAloneIsNotOredWithEveryWeekday(t *testing.T) {
	t.Parallel()
	schedule, err := Parse("0 0 15 * *")
	if err != nil {
		t.Fatal(err)
	}
	got, err := schedule.Next(at(t, "2026-08-24T09:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if want := at(t, "2026-09-15T00:00:00Z"); !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestParseRefusesMalformedExpressions(t *testing.T) {
	t.Parallel()
	bad := map[string]string{
		"empty":                 "",
		"four fields":           "* * * *",
		"six fields":            "* * * * * *",
		"seconds field":         "0 0 3 * * *",
		"daily shorthand":       "@daily",
		"hourly shorthand":      "@hourly",
		"reboot shorthand":      "@reboot",
		"minute out of range":   "60 * * * *",
		"hour out of range":     "* 24 * * *",
		"day out of range":      "* * 32 * *",
		"day zero":              "* * 0 * *",
		"month out of range":    "* * * 13 *",
		"month zero":            "* * * 0 *",
		"weekday out of range":  "* * * * 8",
		"inverted range":        "30-10 * * * *",
		"zero step":             "*/0 * * * *",
		"negative step":         "*/-1 * * * *",
		"empty list entry":      "1,,2 * * * *",
		"trailing comma":        "1,2, * * * *",
		"non numeric":           "abc * * * *",
		"negative value":        "-5 * * * *",
		"bare dash":             "- * * * *",
		"step without base":     "/5 * * * *",
		"double step":           "*/5/5 * * * *",
		"range missing end":     "5- * * * *",
		"name shorthand month":  "0 0 * JAN *",
		"name shorthand day":    "0 0 * * MON",
		"only whitespace":       "     ",
		"tab separated garbage": "*\t*\t*",
	}
	for name, expression := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if schedule, err := Parse(expression); err == nil {
				t.Fatalf("Parse(%q) admitted %+v", expression, schedule)
			}
		})
	}
}

func TestParseNamesWhatItRefused(t *testing.T) {
	t.Parallel()
	_, err := Parse("@daily")
	if err == nil {
		t.Fatal("@daily was admitted")
	}
	if !strings.Contains(err.Error(), "@daily") {
		t.Fatalf("error does not name the input it refused: %v", err)
	}
}

func TestNextGivesUpOnADateThatNeverOccurs(t *testing.T) {
	t.Parallel()
	schedule, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatalf("30 February parses, it simply never occurs: %v", err)
	}
	if _, err := schedule.Next(at(t, "2026-08-24T09:00:00Z")); err == nil {
		t.Fatal("Next found a 30th of February")
	}
}

func TestNextCrossesAYearBoundary(t *testing.T) {
	t.Parallel()
	schedule, err := Parse("0 0 1 1 *")
	if err != nil {
		t.Fatal(err)
	}
	got, err := schedule.Next(at(t, "2026-12-31T23:59:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if want := at(t, "2027-01-01T00:00:00Z"); !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestNextIgnoresTheCallersLocation(t *testing.T) {
	t.Parallel()
	schedule, err := Parse("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	utc := at(t, "2026-08-24T09:15:00Z")
	fromUTC, err := schedule.Next(utc)
	if err != nil {
		t.Fatal(err)
	}
	fromTokyo, err := schedule.Next(utc.In(tokyo))
	if err != nil {
		t.Fatal(err)
	}
	if !fromUTC.Equal(fromTokyo) {
		t.Fatalf("the same instant gave %s from UTC and %s from Tokyo; schedules must be evaluated in UTC",
			fromUTC.Format(time.RFC3339), fromTokyo.Format(time.RFC3339))
	}
}

func TestIntervalMeasuresTheRealGapBetweenFires(t *testing.T) {
	t.Parallel()
	cases := map[string]time.Duration{
		"* * * * *":                   time.Minute,
		"*/1 * * * *":                 time.Minute,
		"0-59 * * * *":                time.Minute,
		"*/30 * * * *":                30 * time.Minute,
		"0 * * * *":                   time.Hour,
		"0 3 * * *":                   24 * time.Hour,
		"0,1,2,3,4,5,6,7,8,9 * * * *": time.Minute,
		"0,30 * * * *":                30 * time.Minute,
	}
	for expression, want := range cases {
		t.Run(expression, func(t *testing.T) {
			t.Parallel()
			schedule, err := Parse(expression)
			if err != nil {
				t.Fatal(err)
			}
			got, err := schedule.Interval(at(t, "2026-08-24T00:00:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("Interval(%q) = %s, want %s", expression, got, want)
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"* * * * *", "0 3 * * *", "*/5 1-4 * * 1,3", "", "@daily", "60 * * * *",
		"0-30/10 * * * *", strings.Repeat("*", 4096),
	} {
		f.Add(seed)
	}
	base := time.Date(2026, 8, 24, 9, 15, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, expression string) {
		schedule, err := Parse(expression)
		if err != nil {
			return
		}
		next, err := schedule.Next(base)
		if err != nil {
			return
		}
		if !next.After(base) {
			t.Fatalf("Parse(%q).Next returned %s, not after %s", expression, next, base)
		}
		if _, err := schedule.Interval(base); err != nil {
			return
		}
	})
}

func TestIntervalIsTheSmallestGapNotTheFirstGap(t *testing.T) {
	t.Parallel()
	schedule, err := Parse("0,1,30 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	got, err := schedule.Interval(at(t, "2026-08-24T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got != time.Minute {
		t.Fatalf("Interval = %s, want 1m: this entry fires at :00 and :01, one minute apart, "+
			"so measuring only the first two fires after midnight would report %s and let it past a minimum-interval bound", got, got)
	}
}
