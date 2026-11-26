package argoworkflow

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestResumeResultConsumedFrozenBeforeInjection(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"{{tasks.producer.outputs.result}}", "{{=steps['producer'].outputs['result']}}", "{{=tasks['producer']['outputs']['result']}}"} {
		w := simpleWorkflow()
		w.Spec.Arguments.Parameters = []wf.Parameter{{Name: "message", Value: wf.AnyStringPtr(text)}}
		plan := holdPlanFor(t, w, requestFor(t, w), Policy{})
		if len(plan.Entries) != 1 || !plan.Entries[0].ResultConsumed {
			t.Fatal("result consumer not frozen", text, plan)
		}
		observed := plan.Clone()
		observed.Entries[0].ResultConsumed = false
		if got := frozenHoldPlan(observed, plan); !got.Entries[0].ResultConsumed {
			t.Fatal("native projection lost authored result fact")
		}
	}
	w := simpleWorkflow()
	plan := holdPlanFor(t, w, requestFor(t, w), Policy{})
	if plan.Entries[0].ResultConsumed {
		t.Fatal("consumer-free control refused")
	}
}
