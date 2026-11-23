package argoworkflow

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestStepsGroupingUsesStructuredIndex(t *testing.T) {
	w := planDAG()
	w.Spec.Templates[0].DAG = nil
	w.Spec.Templates[0].Steps = []wf.ParallelSteps{
		{Steps: []wf.WorkflowStep{{Name: "a", Template: "leaf"}, {Name: "b", Template: "leaf"}}},
		{Steps: []wf.WorkflowStep{{Name: "c", Template: "leaf"}, {Name: "d", Template: "leaf"}}},
	}
	g := newGraph(t.Context(), w, admissioncontract.PrepareRequest{}, Policy{})
	g.build()
	calls := g.nodes[0].calls
	for _, c := range calls {
		c.suffix = "unrelated-presentation"
	}
	ordered, levels := executionOrder(calls)
	p := planner{}
	groups := p.parallelCalls(ordered, levels)
	if len(calls) != 4 || groups[calls[0]] == 0 || groups[calls[0]] != groups[calls[1]] || groups[calls[2]] == 0 || groups[calls[2]] != groups[calls[3]] || groups[calls[0]] == groups[calls[2]] {
		t.Fatal("formatted paths decide grouping", groups)
	}
}
