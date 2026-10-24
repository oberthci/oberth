package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestScheduleEntryParameterRejectsEveryAuthoredValueShape(t *testing.T) {
	for _, p := range []wfv1.Parameter{
		{Name: ScheduleParameter},
		{Name: ScheduleParameter, Value: wfv1.AnyStringPtr("nightly")},
		{Name: ScheduleParameter, Default: wfv1.AnyStringPtr("nightly")},
		{Name: ScheduleParameter, ValueFrom: &wfv1.ValueFrom{Parameter: "other"}},
		{Name: ScheduleParameter, Enum: []wfv1.AnyString{wfv1.AnyString("nightly")}},
	} {
		if len(admitScheduleParameter(&wfv1.WorkflowSpec{Arguments: wfv1.Arguments{Parameters: []wfv1.Parameter{p}}})) == 0 {
			t.Fatalf("authored value accepted: %+v", p)
		}
	}
}
