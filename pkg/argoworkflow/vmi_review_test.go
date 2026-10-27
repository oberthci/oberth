package argoworkflow

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Each graph below runs create-a and create-b concurrently under Argo v4.0.8
// (govaluate shouldExecute, verified separately) for trigger=branch.
func reviewProjection(t *testing.T, wf *wfv1.Workflow, binding RunSourceBinding, policy Policy) {
	t.Helper()
	source, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ResolveRunSource(source, nil, binding)
	if err != nil {
		t.Fatal(err)
	}
	demand, err := r.demand()
	t.Logf("trigger=%s projected=%+v err=%v (real reachable overlap: 2 VMs)", binding.Trigger, demand, err)
	if err != nil || demand != (VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 12 << 30}) {
		t.Fatalf("unsafe expression must fall back to the full two-VM sum: %+v %v", demand, err)
	}
	if _, err := r.Receipt(policy); err != nil {
		t.Fatalf("counterexample must be valid under a sufficient grant: %v", err)
	}
	policy.VMIBudget = VMIBudget{MaxVMs: 1, MaxCores: 2, MaxMemory: resource.MustParse("6Gi")}
	if _, err := r.Receipt(policy); err == nil {
		t.Errorf("UNSOUND: two concurrently live VMIs admitted under a one-VM grant (projected %d)", demand.VMs)
	} else {
		t.Logf("refused under one-VM grant: %v", err)
	}
}

const reviewTernaryFalse = "{{workflow.parameters.oberth-trigger}} == schedule && true ? true : true"
const reviewTernaryTrue = "{{workflow.parameters.oberth-trigger}} == branch || true ? false : false"

func TestReviewHiddenCreateTernary(t *testing.T) {
	wf, binding, policy := vmiSoundnessWorkflow(t)
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
		{Name: "a", Template: "create-a"},
		{Name: "b", Template: "create-b", When: reviewTernaryFalse},
	}}})
	reviewProjection(t, wf, binding, policy)
}

func TestReviewPhantomDeleteLegacyDependencies(t *testing.T) {
	wf, binding, policy := vmiSoundnessWorkflow(t)
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
		{Name: "a", Template: "create-a"},
		{Name: "da", Template: "delete-a", Dependencies: []string{"a"}, When: reviewTernaryTrue},
		{Name: "b", Template: "create-b", Dependencies: []string{"da"}},
	}}})
	reviewProjection(t, wf, binding, policy)
}

func TestReviewPhantomDeleteBareDepends(t *testing.T) {
	wf, binding, policy := vmiSoundnessWorkflow(t)
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
		{Name: "a", Template: "create-a"},
		{Name: "da", Template: "delete-a", Depends: "a.Succeeded", When: reviewTernaryTrue},
		{Name: "b", Template: "create-b", Depends: "da"},
	}}})
	reviewProjection(t, wf, binding, policy)
}

func TestReviewPhantomDeleteSteps(t *testing.T) {
	wf, binding, policy := vmiSoundnessWorkflow(t)
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{
		{Steps: []wfv1.WorkflowStep{{Name: "a", Template: "create-a"}}},
		{Steps: []wfv1.WorkflowStep{{Name: "da", Template: "delete-a", When: reviewTernaryTrue}}},
		{Steps: []wfv1.WorkflowStep{{Name: "b", Template: "create-b"}}},
	}})
	reviewProjection(t, wf, binding, policy)
}

// Control: the same graphs with the parsed-only guards keep one VM (sound).
func TestReviewControlSteps(t *testing.T) {
	wf, binding, policy := vmiSoundnessWorkflow(t)
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", Steps: []wfv1.ParallelSteps{
		{Steps: []wfv1.WorkflowStep{{Name: "a", Template: "create-a"}}},
		{Steps: []wfv1.WorkflowStep{{Name: "da", Template: "delete-a", When: "{{workflow.parameters.oberth-trigger}} == branch"}}},
		{Steps: []wfv1.WorkflowStep{{Name: "b", Template: "create-b"}}},
	}})
	checkVMISoundnessDemand(t, wf, binding, policy, 1)
}

// A delete task named with an expr-lang keyword: every depends expression that
// mentions it fails to compile, so Argo marks the dependent task Errored
// WITHOUT requiring in.Succeeded. The terminal-state successor then runs even
// if the delete failed. The parser accepts "in" as a DNS label.
func TestReviewKeywordTaskNameDependsError(t *testing.T) {
	for _, name := range []string{"in", "not", "and", "or", "if", "let", "nil", "true", "false", "matches", "contains", "else"} {
		t.Run(name, func(t *testing.T) {
			wf, binding, policy := vmiSoundnessWorkflow(t)
			wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
				{Name: "a", Template: "create-a"},
				{Name: name, Template: "delete-a", Depends: "a.Succeeded"},
				{Name: "x", Template: "neutral", Depends: name + ".Succeeded"},
				{Name: "b", Template: "create-b", Depends: "x.Succeeded || x.Failed || x.Errored"},
			}}})
			reviewProjection(t, wf, binding, policy)
		})
	}
}
