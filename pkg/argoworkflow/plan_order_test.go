package argoworkflow

import (
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

// The plan lists steps in the order they run: DAG tasks after everything they
// depend on (depends expressions and dependencies lists), parallel branches in
// declaration order, and a called steps template expanded in place, by groups.
func TestPlanListsStepsInExecutionOrder(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	leaf := wf.Spec.Templates[0]
	leaf.Name = "leaf"
	wf.Spec.Templates = []wfv1.Template{
		{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
			{Name: "release", Template: "leaf", Depends: "test.Succeeded && lint"},
			{Name: "test", Template: "checks", Dependencies: []string{"build"}},
			{Name: "lint", Template: "leaf", Depends: "build"},
			{Name: "build", Template: "leaf"},
			{Name: "docs", Template: "leaf"},
		}}},
		{Name: "checks", Steps: []wfv1.ParallelSteps{
			{Steps: []wfv1.WorkflowStep{{Name: "unit", Template: "leaf"}, {Name: "race", Template: "leaf"}}},
			{Steps: []wfv1.WorkflowStep{{Name: "report", Template: "leaf"}}},
		}},
		leaf,
	}
	plan, err := planFixture(t.Context(), wf, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "docs", "test[0].unit", "test[0].race", "test[1].report", "lint", "release"}
	var got []string
	for _, entry := range plan.Entries {
		got = append(got, entry.Path)
	}
	if !plan.Exact || !reflect.DeepEqual(got, want) {
		t.Fatalf("plan order %q, want %q", got, want)
	}
}

func TestExecutionOrderKeepsDeclarationOrderWithoutDependencies(t *testing.T) {
	t.Parallel()
	steps := []*call{{name: "b"}, {name: "a"}}
	if got, _ := executionOrder(steps); got[0].name != "b" || got[1].name != "a" {
		t.Fatal("steps calls were reordered")
	}
	cycle := []*call{{name: "x", dag: true, after: []string{"y"}}, {name: "y", dag: true, after: []string{"x"}}, {name: "z", dag: true, after: []string{"z", "unknown"}}}
	got, _ := executionOrder(cycle)
	if len(got) != 3 || got[0].name != "z" || got[1].name != "x" || got[2].name != "y" {
		t.Fatalf("cycle or self and unknown references lost a task: %v %v %v", got[0].name, got[1].name, got[2].name)
	}
}
