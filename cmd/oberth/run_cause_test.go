package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// TestRunGetShowsWhereTheRunCameFrom pins the cause lines of oberth run get;
// assistance appears only when it was requested. Wrapping may break a fact
// line, so the check reads the words in order.
func TestRunGetShowsWhereTheRunCameFrom(t *testing.T) {
	for _, tc := range []struct {
		cause      model.RunCause
		want, hide []string
	}{
		{model.RunCause{Actor: "oberth:scheduler", Trigger: model.TriggerSchedule, ScheduleEntry: "nightly", AssistRequested: true},
			[]string{"trigger: schedule", "schedule entry: nightly", "actor: oberth:scheduler", "assist requested: true"}, []string{"rerun of"}},
		{model.RunCause{Actor: "worker", Trigger: model.TriggerBranch, RerunOf: "abcdef0123456789"},
			[]string{"trigger: branch", "rerun of: abcdef0123456789", "actor: worker"}, []string{"assist requested", "schedule entry"}},
	} {
		b := fake.Bindings()
		run := apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "deadbeef", Repo: "origin/team/api", Ref: "refs/heads/main", Status: stepmodel.RunPassed}, RunCause: tc.cause}
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: run}
		app, out, diagnostic := fixture(t, b)
		if code := app.run(t.Context(), []string{"run", "get", "deadbeef"}); code != 0 {
			t.Fatal(code, diagnostic.String())
		}
		text := strings.Join(strings.Fields(out.String()), " ")
		for _, line := range tc.want {
			if !strings.Contains(text, line) {
				t.Fatalf("missing %q in:\n%s", line, out.String())
			}
		}
		for _, absent := range tc.hide {
			if strings.Contains(text, absent) {
				t.Fatalf("unexpected %q in:\n%s", absent, out.String())
			}
		}
	}
}
