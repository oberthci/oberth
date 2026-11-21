package schedule

import (
	"fmt"
	"github.com/oberthci/oberth/internal/publicerr"
	"strings"
	"testing"
	"time"
)

func instant() time.Time { return time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) }
func TestDeclarationRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"empty": "", "unknown": "schedules: [{name: a, cron: '* * * * *', other: true}]", "duplicates": "schedules: [{name: a, cron: '* * * * *'}, {name: a, cron: '* * * * *'}]", "name": "schedules: [{name: '../unsafe', cron: '* * * * *'}]", "too_long_name": "schedules: [{name: '" + strings.Repeat("a", 65) + "', cron: '* * * * *'}]", "branch": "schedules: [{name: a, cron: '* * * * *', branch: '../bad'}]", "timezone": "schedules: [{name: a, cron: '* * * * *', timezone: Local}]", "unknown_timezone": "schedules: [{name: a, cron: '* * * * *', timezone: Etc/Absent}]", "empty_list": "schedules: []", "bad_yaml": "schedules: [", "oversize": strings.Repeat("x", maxSourceBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw), Limits{}, instant()); err == nil {
				t.Fatal("invalid document accepted")
			}
		})
	}
}
func TestEntryLimit(t *testing.T) {
	raw := "schedules:\n"
	for i := range 9 {
		raw += fmt.Sprintf("- name: entry%d\n  cron: '* * * * *'\n", i)
	}
	if _, err := Parse([]byte(raw), Limits{}, instant()); err == nil {
		t.Fatal("default cap disabled")
	}
	file, err := Parse([]byte(raw), Limits{MaxEntries: 9}, instant())
	if err != nil || len(file.Entries) != 9 {
		t.Fatal(file, err)
	}
	for _, limits := range []Limits{{MaxEntries: -1}, {MaxEntries: 257}} {
		if err := limits.Validate(); err == nil {
			t.Fatal("invalid operator limits accepted", limits)
		}
		if _, err := Parse([]byte("schedules: [{name: valid, cron: '* * * * *'}]"), limits, instant()); err == nil {
			t.Fatal("invalid limits accepted", limits)
		}
	}
}
func TestCronRefusalKeepsGoodSiblingAndAuthoredPolicy(t *testing.T) {
	for _, policy := range []string{"", "Allow", "Forbid", "Replace"} {
		raw := fmt.Sprintf("schedules:\n- name: valid+entry\n  cron: '* * * * *'\n  concurrencyPolicy: '%s'\n- name: invalid\n  cron: '@daily'\n", policy)
		file, err := Parse([]byte(raw), Limits{}, instant())
		if err != nil || len(file.Entries) != 1 || len(file.Refused) != 1 {
			t.Fatal(file, err)
		}
		if file.Entries[0].Timezone != "UTC" || file.Entries[0].ConcurrencyPolicy != policy || file.Refused[0].Name != "invalid" {
			t.Fatal(file)
		}
	}
}
func TestNoCalendarOccurrenceAndExpressionBound(t *testing.T) {
	for _, expr := range []string{"0 0 30 2 *", strings.Repeat("*", 257)} {
		file, err := Parse([]byte("schedules: [{name: a, cron: '"+expr+"'}]"), Limits{}, instant())
		if err != nil || len(file.Refused) != 1 {
			t.Fatal(file, err)
		}
	}
}
func TestScheduleMessageCatalogue(t *testing.T) {
	// Reasons and the counted-file template share the bounded catalogue.

	for id := range reasons() {
		failure := invalid(diagnostic(id), "")
		if err := publicerr.CheckMessage(failure.Reason(), 80); err != nil {
			t.Fatal(failure, err)
		}
		if err := publicerr.CheckMessage(failure.Remedy(), 120); err != nil {
			t.Fatal(failure, err)
		}
	}
}

func TestInvalidNativePolicyKeepsGoodSibling(t *testing.T) {
	file, err := Parse([]byte("schedules: [{name: bad, cron: '* * * * *', concurrencyPolicy: forbid}, {name: good, cron: '* * * * *'}]"), Limits{}, instant())
	if err != nil || len(file.Refused) != 1 || len(file.Entries) != 1 || file.Entries[0].Name != "good" {
		t.Fatal(file, err)
	}
}

func TestPublicNumericBoundsAcceptNRefuseNext(t *testing.T) {
	t.Run("document", documentBounds)
	for _, cap := range []int{1, 8, 256} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) { entryBounds(t, cap) })
	}
	t.Run("name", nameBounds)
	t.Run("expression", expressionBounds)
}
func documentBounds(t *testing.T) {
	t.Helper()
	raw := "schedules: [{name: a, cron: '* * * * *'}]\n#"
	raw += strings.Repeat("x", (1<<20)-len(raw))
	if _, err := Parse([]byte(raw), Limits{}, instant()); err != nil {
		t.Fatal(err)
	}
	_, err := Parse([]byte(raw+"x"), Limits{}, instant())
	failure := publicerr.Classify(err)
	if failure == nil || failure.Code() != publicerr.InvalidInput || failure.Reason() != "schedule file is too large" ||
		failure.Remedy() != "Fix the listed values in .oberth/schedule.yaml and push to the default branch." ||
		failure.Detail != `file: field "document" has value 1048577 bytes: the limit is 1048576.` {
		t.Fatal("document bound or whole refusal changed", failure)
	}
}
func entryBounds(t *testing.T, cap int) {
	t.Helper()
	raw := "schedules:\n"
	for i := range cap {
		raw += fmt.Sprintf("- {name: e%d, cron: '* * * * *'}\n", i)
	}
	if _, err := Parse([]byte(raw), Limits{MaxEntries: cap}, instant()); err != nil {
		t.Fatal(err)
	}
	if cap == 8 {
		if _, err := Parse([]byte(raw), Limits{}, instant()); err != nil {
			t.Fatal("default entry cap differs", err)
		}
	}
	if _, err := Parse([]byte(raw+"- {name: excess, cron: '* * * * *'}\n"), Limits{MaxEntries: cap}, instant()); err == nil {
		t.Fatal("entry cap removed")
	}
}
func nameBounds(t *testing.T) {
	t.Helper()
	for _, size := range []int{64, 65} {
		_, err := Parse([]byte("schedules: [{name: '"+strings.Repeat("a", size)+"', cron: '* * * * *'}]"), Limits{}, instant())
		if (err == nil) != (size == 64) {
			t.Fatal(size, err)
		}
	}
}
func expressionBounds(t *testing.T) {
	t.Helper()
	for _, size := range []int{256, 257} {
		expression := "0 0 * * *" + strings.Repeat(" ", size-len("0 0 * * *"))
		file, err := Parse([]byte("schedules: [{name: a, cron: '"+expression+"'}]"), Limits{}, instant())
		if err != nil || (len(file.Refused) == 0) != (size == 256) {
			t.Fatal(size, file, err)
		}
	}
}
func TestAuthoredExpressionAndSundayNormalization(t *testing.T) {
	file, err := Parse([]byte("schedules: [{name: sunday, cron: '0 3 * * 7', timezone: Europe/Amsterdam}]"), Limits{}, instant())
	if err != nil || len(file.Entries) != 1 {
		t.Fatal(file, err)
	}
	entry := file.Entries[0]
	if entry.Cron != "0 3 * * 7" || entry.CronExpression() != "0 3 * * 0" {
		t.Fatal("authored/native syntax differs", entry)
	}
}

func TestRefusedEntryCountsEveryProblemOnceAndKeepsGoodSibling(t *testing.T) {
	file, err := Parse([]byte("schedules: [{name: bad, cron: '@daily', concurrencyPolicy: bad}, {name: good, cron: '0 3 * * *'}]"), Limits{}, instant())
	if err != nil || len(file.Refused) != 1 || len(file.Entries) != 1 || file.Entries[0].Name != "good" {
		t.Fatal(file, err)
	}
	refusal := publicerr.Classify(file.Refused[0].Error)
	want := `entry #1 ("bad"): field "concurrencyPolicy" has value "bad": use Allow, Forbid, Replace, or omit the field.
entry #1 ("bad"): field "cron" has value "@daily": use five numeric cron fields within 256 bytes.`
	if refusal.Reason() != "2 problems in .oberth/schedule.yaml" || refusal.Remedy() != "Fix the listed values in .oberth/schedule.yaml and push to the default branch." || refusal.Detail != want {
		t.Fatal(refusal)
	}
}
