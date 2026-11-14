package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestScheduleEntryAlphabet(t *testing.T) {
	for _, entry := range []string{"nightly", "Weekly.1_2/3+4@5=6,7-8", strings.Repeat("a", 64), ""} {
		if err := ValidateScheduleEntry(RunTriggerSchedule, entry); err != nil {
			t.Fatalf("safe schedule entry %q refused: %v", entry, err)
		}
	}
	for _, entry := range []string{"night ly", "night'ly", "{{workflow.name}}", "$(id)", "night;ly", "night\nly", "夜間", "-nightly", strings.Repeat("a", 65)} {
		if err := SetScheduleParameter(&wfv1.Workflow{}, RunTriggerSchedule, entry); err == nil {
			t.Fatalf("unsafe schedule entry became an Argo parameter: %q", entry)
		}
	}
}

func TestScheduleEntryParameterRejectsEveryAuthoredValueShape(t *testing.T) {
	for _, p := range []wfv1.Parameter{
		{Name: ScheduleParameter},
		{Name: ScheduleParameter, Value: wfv1.AnyStringPtr("nightly")},
		{Name: ScheduleParameter, Default: wfv1.AnyStringPtr("nightly")},
		{Name: ScheduleParameter, ValueFrom: &wfv1.ValueFrom{Parameter: "other"}},
		{Name: ScheduleParameter, Enum: []wfv1.AnyString{wfv1.AnyString("nightly")}},
	} {
		if len(admitParameters("spec.arguments.parameters", []wfv1.Parameter{p})) == 0 {
			t.Fatalf("authored value accepted: %+v", p)
		}
	}
}
