package argoworkflow

import (
	"fmt"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestRefusalPlannerBoundsCompositeGraphs(t *testing.T) {
	for _, shape := range []string{"complete-13", "diamond-40"} {
		t.Run(shape, func(t *testing.T) {
			workflow := refusalCompositeGraph(shape)
			if err := Admit(workflow, Policy{}); err != nil {
				t.Fatal(err)
			}
			type result struct {
				steps []PlannedStep
				err   error
			}
			done := make(chan result, 1)
			go func() { steps, err := PlannedSteps(workflow); done <- result{steps, err} }()
			select {
			case got := <-done:
				if got.err != nil || len(got.steps) != 0 {
					t.Fatalf("composite-only graph: %d rows, %v", len(got.steps), got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("composite graph enumeration exceeded two seconds")
			}
		})
	}
}

func refusalCompositeGraph(shape string) *wfv1.Workflow {
	w := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "t0"}}
	count := 13
	if shape == "diamond-40" {
		count = 40
	}
	for i := 0; i < count; i++ {
		var steps []wfv1.WorkflowStep
		if shape == "complete-13" {
			for j := 0; j < count; j++ {
				steps = append(steps, wfv1.WorkflowStep{Name: fmt.Sprintf("s%d", j), Template: fmt.Sprintf("t%d", j)})
			}
		} else {
			next := fmt.Sprintf("t%d", i+1)
			steps = []wfv1.WorkflowStep{{Name: "a", Template: next}, {Name: "b", Template: next}}
		}
		w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: fmt.Sprintf("t%d", i), Steps: []wfv1.ParallelSteps{{Steps: steps}}})
	}
	if shape == "diamond-40" {
		w.Spec.Templates = append(w.Spec.Templates, wfv1.Template{Name: "t40", DAG: &wfv1.DAGTemplate{}})
	}
	return w
}
