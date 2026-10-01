package schedule

import (
	"strings"
	"testing"
	"time"
)

func testLimits() Limits {
	return Limits{MinInterval: 15 * time.Minute, MaxEntries: 4}
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.UTC()
}

const twoGood = `
schedules:
  - name: nightly
    cron: "0 3 * * *"
    branch: main
  - name: weekly
    cron: "0 4 * * 1"
`

func TestParseReadsEntries(t *testing.T) {
	t.Parallel()
	file, err := Parse([]byte(twoGood), testLimits(), at(t, "2026-08-24T00:00:00Z"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(file.Entries) != 2 {
		t.Fatalf("parsed %d entries: %v", len(file.Entries), names(file.Entries))
	}
	if file.Entries[0].Name != "nightly" || file.Entries[0].Branch != "main" {
		t.Fatalf("entry name=%q branch=%q", file.Entries[0].Name, file.Entries[0].Branch)
	}
	if file.Entries[1].Branch != "" {
		t.Fatalf("an entry with no branch should stay empty so the caller can default it: %q", file.Entries[1].Branch)
	}
	if len(file.Refused) != 0 {
		t.Fatalf("refused %+v", file.Refused)
	}
}

func TestParseRefusesAnEntryUnderTheMinimumIntervalAndKeepsItsSiblings(t *testing.T) {
	t.Parallel()
	raw := `
schedules:
  - name: hammer
    cron: "* * * * *"
  - name: nightly
    cron: "0 3 * * *"
`
	file, err := Parse([]byte(raw), testLimits(), at(t, "2026-08-24T00:00:00Z"))
	if err != nil {
		t.Fatalf("one bad entry must not fail the file: %v", err)
	}
	if len(file.Entries) != 1 || file.Entries[0].Name != "nightly" {
		t.Fatalf("surviving entries = %v", names(file.Entries))
	}
	if len(file.Refused) != 1 || file.Refused[0].Name != "hammer" {
		t.Fatalf("refusals = %+v", file.Refused)
	}
	if !strings.Contains(file.Refused[0].Reason, "15m") {
		t.Fatalf("refusal does not name the minimum it enforced: %q", file.Refused[0].Reason)
	}
}

func TestParseCatchesAScheduleThatHidesItsRealInterval(t *testing.T) {
	t.Parallel()
	raw := `
schedules:
  - name: sneaky
    cron: "0,1,30 * * * *"
`
	file, err := Parse([]byte(raw), testLimits(), at(t, "2026-08-24T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Entries) != 0 {
		t.Fatal("an entry firing at :00 and :01 was admitted against a 15 minute minimum")
	}
	if len(file.Refused) != 1 {
		t.Fatalf("refusals = %+v", file.Refused)
	}
}

func TestParseRefusesAFileOverTheEntryCap(t *testing.T) {
	t.Parallel()
	var builder strings.Builder
	builder.WriteString("schedules:\n")
	for index := 0; index < 5; index++ {
		builder.WriteString("  - name: e")
		builder.WriteString(string(rune('a' + index)))
		builder.WriteString("\n    cron: \"0 3 * * *\"\n")
	}
	_, err := Parse([]byte(builder.String()), testLimits(), at(t, "2026-08-24T00:00:00Z"))
	if err == nil {
		t.Fatal("a file over the entry cap was admitted")
	}
	if !strings.Contains(err.Error(), "4") {
		t.Fatalf("error does not name the cap: %v", err)
	}
}

func TestParseRefusesMalformedFiles(t *testing.T) {
	t.Parallel()
	bad := map[string]string{
		"unknown field":                      "schedules:\n  - name: a\n    cron: \"0 3 * * *\"\n    timezone: Europe/Amsterdam\n",
		"not yaml":                           "schedules: [",
		"no schedules key":                   "jobs:\n  - name: a\n",
		"entry without name":                 "schedules:\n  - cron: \"0 3 * * *\"\n",
		"entry without cron":                 "schedules:\n  - name: a\n",
		"duplicate names":                    "schedules:\n  - name: a\n    cron: \"0 3 * * *\"\n  - name: a\n    cron: \"0 4 * * *\"\n",
		"empty document":                     "",
		"branch with a slash in a bad place": "schedules:\n  - name: a\n    cron: \"0 3 * * *\"\n    branch: \"../escape\"\n",
	}
	for name, raw := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if file, err := Parse([]byte(raw), testLimits(), at(t, "2026-08-24T00:00:00Z")); err == nil {
				t.Fatalf("Parse admitted %v", names(file.Entries))
			}
		})
	}
}

func TestParseRefusesAnOversizeDocument(t *testing.T) {
	t.Parallel()
	raw := "schedules:\n  - name: a\n    cron: \"0 3 * * *\"\n#" + strings.Repeat("x", MaxSourceBytes)
	if _, err := Parse([]byte(raw), testLimits(), at(t, "2026-08-24T00:00:00Z")); err == nil {
		t.Fatal("an oversize document was admitted")
	}
}

func TestParseRefusesAnUnparseableExpressionByName(t *testing.T) {
	t.Parallel()
	raw := `
schedules:
  - name: broken
    cron: "not a cron"
  - name: fine
    cron: "0 3 * * *"
`
	file, err := Parse([]byte(raw), testLimits(), at(t, "2026-08-24T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Entries) != 1 || file.Entries[0].Name != "fine" {
		t.Fatalf("entries = %v", names(file.Entries))
	}
	if len(file.Refused) != 1 || file.Refused[0].Name != "broken" {
		t.Fatalf("refusals = %+v", file.Refused)
	}
}

func TestDueReturnsAnEntryWhoseTimeHasPassed(t *testing.T) {
	t.Parallel()
	file, err := Parse([]byte(twoGood), testLimits(), at(t, "2026-08-24T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]time.Time{"nightly": at(t, "2026-08-23T03:00:00Z"), "weekly": at(t, "2026-08-24T04:00:00Z")}
	due := file.Due(last, at(t, "2026-08-24T03:05:00Z"))
	if len(due) != 1 || due[0].Name != "nightly" {
		t.Fatalf("due = %v", names(due))
	}
}

func TestDueReturnsAtMostOneFirePerEntryHoweverLongTheOutage(t *testing.T) {
	t.Parallel()
	file, err := Parse([]byte(twoGood), testLimits(), at(t, "2026-08-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]time.Time{"nightly": at(t, "2026-08-01T03:00:00Z")}
	due := file.Due(last, at(t, "2026-08-24T03:05:00Z"))
	names := map[string]int{}
	for _, entry := range due {
		names[entry.Name]++
	}
	if names["nightly"] != 1 {
		t.Fatalf("23 days of missed fires produced %d nightly runs, want exactly 1", names["nightly"])
	}
}

func TestDueReturnsNothingBeforeTheFirstFire(t *testing.T) {
	t.Parallel()
	file, err := Parse([]byte(twoGood), testLimits(), at(t, "2026-08-24T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]time.Time{"nightly": at(t, "2026-08-24T03:00:00Z")}
	if due := file.Due(last, at(t, "2026-08-24T03:01:00Z")); len(due) != 0 {
		t.Fatalf("due one minute after firing: %v", names(due))
	}
}

func TestDueTreatsAnEntryWithNoHistoryAsDueOnlyOnceItsTimeArrives(t *testing.T) {
	t.Parallel()
	file, err := Parse([]byte(twoGood), testLimits(), at(t, "2026-08-24T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if due := file.Due(nil, at(t, "2026-08-24T00:30:00Z")); len(due) != 0 {
		t.Fatalf("a never-fired entry ran immediately at startup: %v", names(due))
	}
	due := names(file.Due(nil, at(t, "2026-08-25T03:30:00Z")))
	if !due["nightly"] {
		t.Fatalf("nightly never became due; got %v", due)
	}
}

func names(entries []Entry) map[string]bool {
	found := map[string]bool{}
	for _, entry := range entries {
		found[entry.Name] = true
	}
	return found
}
