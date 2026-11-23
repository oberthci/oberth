package argoworkflow

import (
	"slices"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestPlanGroupsStayConsecutiveBesideChains(t *testing.T) {
	w := planDAG()
	w.Spec.Templates[0].DAG.Tasks = []wf.DAGTask{
		{Name: "a", Template: "leaf"},
		{Name: "b", Template: "leaf", Dependencies: []string{"a"}},
		{Name: "e", Template: "leaf", Dependencies: []string{"b"}},
		{Name: "c", Template: "leaf", Dependencies: []string{"a"}},
		{Name: "d", Template: "leaf", Dependencies: []string{"c"}},
	}
	p, err := planFixture(t.Context(), w, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(planPaths(p), []string{"a", "b", "c", "e", "d"}) {
		t.Fatal("dependency levels are interleaved", planPaths(p))
	}
	if p.Entries[1].ParallelGroup == 0 || p.Entries[1].ParallelGroup != p.Entries[2].ParallelGroup || p.Entries[3].ParallelGroup == 0 || p.Entries[3].ParallelGroup != p.Entries[4].ParallelGroup || p.Entries[1].ParallelGroup == p.Entries[3].ParallelGroup {
		t.Fatal("consecutive dependency levels do not have distinct groups", p)
	}
}

func TestRuntimeLeafFanOutHasAGroupAtAdmission(t *testing.T) {
	for _, withParam := range []bool{false, true} {
		w := planDAG()
		task := &w.Spec.Templates[0].DAG.Tasks[0]
		if withParam {
			task.WithParam = "{{workflow.parameters.items}}"
		} else {
			task.WithItems = loopItems()
			task.WithItems[0].Value = []byte(`"{{workflow.parameters.item}}"`)
		}
		p, err := planFixture(t.Context(), w, admissionFixture{})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Entries) != 1 || !p.Entries[0].Dynamic || !p.Entries[0].FanOut || p.Entries[0].ParallelGroup == 0 {
			t.Fatal("runtime loop has no admitted group", p)
		}
	}
}

func TestPlanParallelGroupsComeFromDependencyLevels(t *testing.T) {
	t.Parallel()
	w := planDAG()
	w.Spec.Templates[0].DAG.Tasks = []wf.DAGTask{
		{Name: "profiles", Template: "leaf"},
		{Name: "release-index", Template: "leaf", Dependencies: []string{"profiles"}},
		{Name: "tools-dir", Template: "leaf", Depends: "profiles.Succeeded"},
		{Name: "deprecations", Template: "leaf", Depends: "profiles"},
		{Name: "finish", Template: "leaf", Dependencies: []string{"release-index", "tools-dir", "deprecations"}},
	}
	p, err := planFixture(t.Context(), w, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 5 || p.Entries[0].ParallelGroup != 0 || p.Entries[4].ParallelGroup != 0 {
		t.Fatal("serial steps grouped", p)
	}
	group := p.Entries[1].ParallelGroup
	if group == 0 || p.Entries[2].ParallelGroup != group || p.Entries[3].ParallelGroup != group {
		t.Fatal("parallel branches have no common group at admission", p)
	}
}

func TestPlanParallelGroupsRespectNestedSerialStepsAndHooks(t *testing.T) {
	t.Parallel()
	w := planDAG()
	w.Spec.Templates[0].DAG.Tasks = []wf.DAGTask{
		{Name: "one", Template: "leaf"}, {Name: "two", Template: "leaf"},
		{Name: "nested", Template: "sequence", Dependencies: []string{"one", "two"}},
		{Name: "loop", Template: "leaf", Dependencies: []string{"nested"}, WithItems: loopItems()},
		{Name: "hooked", Template: "leaf", Dependencies: []string{"loop"}, OnExit: "leaf"},
	}
	w.Spec.Templates = append(w.Spec.Templates, wf.Template{Name: "sequence", Steps: []wf.ParallelSteps{
		{Steps: []wf.WorkflowStep{{Name: "a", Template: "leaf"}, {Name: "b", Template: "leaf"}}},
		{Steps: []wf.WorkflowStep{{Name: "c", Template: "leaf"}}},
	}})
	p, err := planFixture(t.Context(), w, admissionFixture{})
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 1, 2, 2, 0, 3, 3, 0, 0}
	if len(p.Entries) != len(want) {
		t.Fatal(p)
	}
	for i, e := range p.Entries {
		if e.ParallelGroup != want[i] {
			t.Fatalf("%s group %d, want %d", e.Path, e.ParallelGroup, want[i])
		}
	}
}

func TestParallelLevelsIncludeDifferentDependenciesAndKeepPlanOrder(t *testing.T) {
	t.Parallel()
	p := planner{}
	leaf := &node{template: &wf.Template{Suspend: &wf.SuspendTemplate{}}}
	calls := []*call{
		{name: "a", dag: true, target: leaf},
		{name: "b", dag: true, target: leaf},
		{name: "c", dag: true, target: leaf, after: []string{"a"}},
		{name: "d", dag: true, target: leaf, after: []string{"b", "Succeeded"}},
		{name: "e", dag: true, target: leaf, after: []string{"c", "d"}},
	}
	ordered, levels := executionOrder(calls)
	groups := p.parallelCalls(ordered, levels)
	if groups[calls[0]] == 0 || groups[calls[0]] != groups[calls[1]] || groups[calls[2]] == 0 || groups[calls[2]] != groups[calls[3]] || groups[calls[4]] != 0 || groups[calls[0]] == groups[calls[2]] {
		t.Fatal("groups do not follow dependency levels", groups)
	}
}
