package argoworkflow

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestTemplateParallelismOneSuppressesGroups(t *testing.T) {
	for _, steps := range []bool{false, true} {
		w := planDAG()
		tasks := []wf.DAGTask{{Name: "a", Template: "leaf"}, {Name: "b", Template: "leaf"},
			{Name: "fan", Template: "leaf", WithParam: "{{workflow.parameters.items}}"},
			{Name: "literal", Template: "leaf", WithItems: loopItems()}}
		template := &w.Spec.Templates[0]
		one := int64(1)
		template.Parallelism, template.DAG.Tasks = &one, tasks
		if steps {
			template.DAG = nil
			group := wf.ParallelSteps{}
			for _, task := range tasks {
				group.Steps = append(group.Steps, wf.WorkflowStep{Name: task.Name, Template: task.Template, WithParam: task.WithParam, WithItems: task.WithItems})
			}
			template.Steps = []wf.ParallelSteps{group}
		}
		plan, err := planFixture(t.Context(), w, admissionFixture{})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Entries) != 5 {
			t.Fatal("serial loop lost entries", plan)
		}
		for _, entry := range plan.Entries {
			if entry.ParallelGroup != 0 {
				t.Fatal("serial template has a parallel group", steps, plan)
			}
		}
	}
}

func TestMutuallyExclusiveWhenBranchesKeepStaticGrouping(t *testing.T) {
	w := planDAG()
	w.Spec.Templates[0].DAG.Tasks = []wf.DAGTask{
		{Name: "a", Template: "leaf", When: "{{workflow.parameters.mode}} == a"},
		{Name: "b", Template: "leaf", When: "{{workflow.parameters.mode}} == b"},
	}
	plan, err := planFixture(t.Context(), w, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Entries[0].ParallelGroup == 0 || plan.Entries[0].ParallelGroup != plan.Entries[1].ParallelGroup {
		t.Fatal("conditions changed static grouping", plan)
	}
}
