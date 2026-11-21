package argoworkflow

import (
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"k8s.io/apimachinery/pkg/util/intstr"
	"strings"
	"testing"
)

func TestHoldInvocationContexts(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"dag", "steps"} {
		for _, route := range []string{"static", "own retry", "parent retry", "outer retry", "workflow retry", "default retry", "items", "param", "sequence"} {
			t.Run(shape+"/"+route, func(t *testing.T) { checkHoldInvocationContext(t, shape, route) })
		}
	}
}
func checkHoldInvocationContext(t *testing.T, shape, route string) {
	t.Helper()
	w := holdInvocationWorkflow(shape)
	group := &w.Spec.Templates[1]
	retry := &wf.RetryStrategy{Limit: new(intstr.FromInt(1))}
	want := ""
	switch route {
	case "own retry":
		w.Spec.Templates[0].RetryStrategy = retry
	case "parent retry":
		group.RetryStrategy = retry
		want = holdRetriedGroup
	case "outer retry":
		outer := wf.Template{Name: "outer", DAG: &wf.DAGTemplate{Tasks: []wf.DAGTask{{Name: "outer", Template: "group"}}}, RetryStrategy: retry}
		w.Spec.Templates = append(w.Spec.Templates, outer)
		w.Spec.Entrypoint = "outer"
		want = holdRetriedGroup
	case "workflow retry":
		w.Spec.RetryStrategy = retry
		want = holdRetriedGroup
	case "default retry":
		w.Spec.TemplateDefaults = &wf.Template{RetryStrategy: retry}
		want = holdRetriedGroup
	case "items", "param", "sequence":
		setHoldInvocationLoop(group, shape, route)
		want = holdLoop
	}
	p, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
	if err != nil {
		t.Fatal("eligibility changed admission", err)
	}
	for _, entry := range p.View().Plan.Entries {
		if entry.HoldEligible != (want == "") || entry.HoldRefusal != want {
			t.Fatal("wrong invocation eligibility", route, entry, want)
		}
	}
}
func holdInvocationWorkflow(shape string) *wf.Workflow {
	w := simpleWorkflow()
	w.Spec.Entrypoint = "group"
	group := wf.Template{Name: "group"}
	if shape == "dag" {
		group.DAG = &wf.DAGTemplate{Tasks: []wf.DAGTask{{Name: "build", Template: "main"}}}
	} else {
		group.Steps = []wf.ParallelSteps{{Steps: []wf.WorkflowStep{{Name: "build", Template: "main"}}}}
	}
	w.Spec.Templates = append(w.Spec.Templates, group)
	return w
}
func setHoldInvocationLoop(t *wf.Template, shape, route string) {
	items := []wf.Item{{Value: []byte(`"a"`)}, {Value: []byte(`"b"`)}}
	sequence := &wf.Sequence{Count: new(intstr.FromInt(2))}
	if shape == "dag" {
		c := &t.DAG.Tasks[0]
		switch route {
		case "items":
			c.WithItems = items
		case "param":
			c.WithParam = `["a","b"]`
		case "sequence":
			c.WithSequence = sequence
		}
	} else {
		c := &t.Steps[0].Steps[0]
		switch route {
		case "items":
			c.WithItems = items
		case "param":
			c.WithParam = `["a","b"]`
		case "sequence":
			c.WithSequence = sequence
		}
	}
}
func TestHoldAllUsesIncludesBoundedSubtree(t *testing.T) {
	t.Parallel()
	w := holdInvocationWorkflow("dag")
	group := &w.Spec.Templates[1]
	group.DAG.Tasks = nil
	for i := range admissioncontract.MaxPlanEntries + 1 {
		group.DAG.Tasks = append(group.DAG.Tasks, wf.DAGTask{Name: fmt.Sprintf("call-%d", i), Template: "filler"})
	}
	group.DAG.Tasks = append(group.DAG.Tasks, wf.DAGTask{Name: "unvisited", Template: "main"})
	filler := w.Spec.Templates[0].DeepCopy()
	filler.Name = "filler"
	w.Spec.Templates = append(w.Spec.Templates, *filler)
	w.Spec.Templates = append(w.Spec.Templates, wf.Template{Name: "outer", DAG: &wf.DAGTemplate{Tasks: []wf.DAGTask{{Name: "direct", Template: "main"}, {Name: "hidden", Template: "group"}}}})
	w.Spec.Entrypoint = "outer"
	plan := holdPlanFor(t, w, requestFor(t, w), Policy{})
	if plan.Exact || len(plan.Entries) != 2 {
		t.Fatal("missing bounded subtree", plan)
	}
	for _, entry := range plan.Entries {
		if entry.HoldEligible || entry.HoldRefusal != holdSharedUse {
			t.Fatal("unrepresented use allowed assistance", entry)
		}
	}
}
func TestHoldInvocationRefusalsAreBounded(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{holdRetriedGroup, holdLoop, holdSharedUse} {
		if len(reason) > 80 || len("assist not used: "+reason) > 80 || strings.Contains(reason, "...") {
			t.Fatal("unbounded refusal", reason)
		}
	}
}

func TestHoldUsesIncludeDynamicCandidatesButIgnoreInspectionOnly(t *testing.T) {
	t.Parallel()
	leaf := &node{key: "build", template: simpleWorkflow().Spec.Templates[0].DeepCopy()}
	for _, inspectionOnly := range []bool{false, true} {
		g := &graph{workflow: simpleWorkflow(), roots: []*call{{target: leaf}, {dynamic: true, inspectionOnly: inspectionOnly, candidates: []*node{leaf}}}}
		reasons := g.holdInvocationRefusals(admissioncontract.Plan{}, nil)
		want := holdSharedUse
		if inspectionOnly {
			want = ""
		}
		if reasons["build"] != want {
			t.Fatal("wrong resolved-use coverage", inspectionOnly, reasons)
		}
	}
}

func TestHoldAllUsesIncludesHiddenInvocationHooks(t *testing.T) {
	t.Parallel()
	for _, hidden := range []bool{false, true} {
		leaf := &node{key: "build", template: simpleWorkflow().Spec.Templates[0].DeepCopy()}
		group := &node{key: "group", template: &wf.Template{Name: "group", DAG: &wf.DAGTemplate{}}}
		invocation := &call{target: group, hooks: []*call{{target: leaf}}}
		g := &graph{workflow: simpleWorkflow(), byKey: map[string]*node{"group": group}, roots: []*call{{target: leaf}, invocation}}
		plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "direct", Template: "build"}, {Path: "hidden", Template: "group", Dynamic: true}}}
		omitted := map[*call]bool{}
		want := ""
		if hidden {
			omitted[invocation] = true
			want = holdSharedUse
		}
		if reasons := g.holdInvocationRefusals(plan, omitted); reasons["build"] != want {
			t.Fatal("wrong invocation hook coverage", hidden, reasons)
		}
	}
}

func TestHoldPlannerRecordsOmittedInvocation(t *testing.T) {
	t.Parallel()
	leaf := &node{key: "build", template: simpleWorkflow().Spec.Templates[0].DeepCopy()}
	group := &node{key: "group", template: &wf.Template{Name: "group", DAG: &wf.DAGTemplate{}}}
	invocation := &call{target: group, hooks: []*call{{target: leaf}, {target: leaf}}}
	omitted := map[*call]bool{}
	p := planner{omitted: omitted, active: map[*node]bool{}, plan: admissioncontract.Plan{Entries: make([]admissioncontract.PlanEntry, admissioncontract.MaxPlanEntries-1)}}
	if !p.invoke(invocation, "hidden") || !omitted[invocation] {
		t.Fatal("whole invocation placeholder did not fence hidden hooks", p.plan, omitted)
	}
	g := &graph{workflow: simpleWorkflow(), roots: []*call{{target: leaf}, invocation}}
	if reasons := g.holdInvocationRefusals(admissioncontract.Plan{}, omitted); reasons["build"] != holdSharedUse {
		t.Fatal("unrepresented hook use allowed assistance", reasons)
	}
}
