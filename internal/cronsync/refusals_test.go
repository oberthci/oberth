package cronsync_test

import (
	"fmt"
	"testing"
)

const fileRemedy = "Fix the listed values in .oberth/schedule.yaml and push to the default branch."

func TestPublicScheduleRefusalsNameOffenderAndRemedy(t *testing.T) {
	for _, row := range []struct{ name, raw, reason, detail, entry string }{
		{"unknown_branch", "schedules: [{name: nightly, cron: '0 3 * * *', branch: absent}]", "schedule branch not found", "schedule branch not found - Push the branch to the oberth remote: git push oberth <branch>.\nentry \"nightly\": field \"branch\" has value \"absent\": push this branch to the oberth remote or choose one already there.", "nightly"},
		{"bad_cron", "schedules: [{name: nightly, cron: '@daily'}]", "schedule file is invalid", "schedule cron expression is invalid - " + fileRemedy + "\nentry #1 (\"nightly\"): field \"cron\" has value \"@daily\": use five numeric cron fields within 256 bytes.", "nightly"},
		{"bad_timezone", "schedules: [{name: nightly, cron: '0 3 * * *', timezone: Local}]", "schedule file is invalid", "schedule time zone is invalid - " + fileRemedy + "\nentry #1 (\"nightly\"): field \"timezone\" has value \"Local\": name a time zone or omit it for UTC.", ""},
		{"duplicate", "schedules: [{name: nightly, cron: '0 3 * * *'}, {name: nightly, cron: '0 4 * * *'}]", "schedule file is invalid", "schedule name is repeated - " + fileRemedy + "\nentry #2 (\"nightly\"): field \"name\" has value \"nightly\": choose a unique entry name.", ""},
		{"cap", nineEntries(), "schedule file is invalid", "too many schedules - " + fileRemedy + "\nfile: field \"schedules\" has value 9 entries: the limit is 8.", ""},
		{"empty_list", "schedules: []", "schedule file is invalid", "schedule list is empty - " + fileRemedy + "\nfile: field \"schedules\" has value []: add at least one entry.", ""},
		{"unknown_field", "schedules: [{name: nightly, cron: '0 3 * * *', mystery: true}]", "schedule file is invalid", "schedule field is unknown - " + fileRemedy + "\nentry #1 (\"nightly\"): field \"mystery\" has value \"true\": remove this unknown field.", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t)
			f.put(f.repo, row.raw)
			f.sync()
			statuses, err := f.c.Snapshot(t.Context(), f.repo)
			if err != nil || len(statuses) != 1 {
				t.Fatal(statuses, err)
			}
			got := statuses[0]
			if got.Entry != row.entry || got.Error != row.reason || got.Detail != row.detail {
				t.Fatalf("got %#v\nwant %q\n%q", got, row.reason, row.detail)
			}
			if len(f.native.Actions()) != 0 {
				t.Fatal("author refusal did not converge without applying work")
			}
		})
	}
}
func nineEntries() string {
	raw := "schedules:\n"
	for i := range 9 {
		raw += fmt.Sprintf("- {name: e%d, cron: '0 3 * * *'}\n", i)
	}
	return raw
}
func TestPublicFileRefusalReportsAllProblemsTogether(t *testing.T) {
	f := newFixture(t)
	f.put(f.repo, `extra: bad
schedules:
- {name: nightly, cron: '@daily', timezone: Local, mystery: true}
- {name: nightly, cron: '0 3 * * *', branch: '../bad'}
`)
	f.sync()
	statuses, err := f.c.Snapshot(t.Context(), f.repo)
	if err != nil || len(statuses) != 1 {
		t.Fatal(statuses, err)
	}
	want := "6 problems in .oberth/schedule.yaml - " + fileRemedy + `
file: field "extra" has value "bad": remove this unknown field.
entry #1 ("nightly"): field "timezone" has value "Local": name a time zone or omit it for UTC.
entry #1 ("nightly"): field "mystery" has value "true": remove this unknown field.
entry #1 ("nightly"): field "cron" has value "@daily": use five numeric cron fields within 256 bytes.
entry #2 ("nightly"): field "name" has value "nightly": choose a unique entry name.
entry #2 ("nightly"): field "branch" has value "../bad": use a public branch name such as main.`
	if statuses[0].Error != "schedule file is invalid" || statuses[0].Detail != want {
		t.Fatalf("got %#v\nwant %q", statuses[0], want)
	}
}
func TestTrimmedNameAndBranchReachAdmission(t *testing.T) {
	f := newFixture(t)
	f.put(f.repo, "schedules: [{name: ' nightly ', cron: '* * * * *', branch: ' main '}]")
	f.sync()
	if len(f.calls) != 1 || f.calls[0].Entry.Name != "nightly" || f.calls[0].Entry.Branch != "main" {
		t.Fatal(f.calls)
	}
	if got := f.object(f.repo, "nightly"); got.Spec.WorkflowMetadata.Annotations["oberth.ci/ref"] != "refs/heads/main" {
		t.Fatal(got)
	}
}
