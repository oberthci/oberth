package argoworkflow

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/expr/argoexpr"
	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func vmiSoundnessWorkflow(t *testing.T) (*wfv1.Workflow, RunSourceBinding, Policy) {
	t.Helper()
	source, _, binding, policy := runVMIFixture(t)
	wf, err := Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	a := runVMITemplate(wf, "create-a").DeepCopy()
	b := runVMITemplate(wf, "create-b").DeepCopy()
	da := runVMITemplate(wf, "delete-a").DeepCopy()
	wf.Spec.Templates = []wfv1.Template{*a, *b, *da, {Name: "neutral", Container: &corev1.Container{Image: "busybox:1.37@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Command: []string{"true"}}}}
	wf.Spec.Entrypoint = "main"
	wf.Spec.OnExit = ""
	return wf, binding, policy
}

func checkVMISoundnessDemand(t *testing.T, wf *wfv1.Workflow, binding RunSourceBinding, policy Policy, want int) {
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
	if err != nil || demand.VMs != want {
		t.Fatalf("projected %+v, err=%v; actual reachable overlap requires %d VMs", demand, err, want)
	}
	if _, err := r.Receipt(policy); err != nil {
		t.Fatalf("counterexample must be policy-valid at a sufficient grant: %v", err)
	}
	if want == 2 {
		policy.VMIBudget = VMIBudget{MaxVMs: 1, MaxCores: 2, MaxMemory: resource.MustParse("6Gi")}
		if _, err := r.Receipt(policy); err == nil {
			t.Fatal("two reachable live VMIs admitted under a one-VM grant")
		}
	}
}

func TestRunVMIDependsPrecedenceSoundness(t *testing.T) {
	for _, expression := range []string{
		"x.Succeeded || x.Failed || x.Errored && da.Succeeded",
		"da.Succeeded && x.Succeeded || x.Failed || x.Errored",
		"(x.Succeeded || x.Failed || x.Errored) && da.Succeeded",
	} {
		t.Run(expression, func(t *testing.T) {
			wf, binding, policy := vmiSoundnessWorkflow(t)
			wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{FailFast: ptr.To(false), Tasks: []wfv1.DAGTask{
				{Name: "a", Template: "create-a"},
				{Name: "x", Template: "neutral"},
				{Name: "da", Template: "delete-a", Depends: "a.Succeeded"},
				{Name: "b", Template: "create-b", Depends: expression},
			}}})
			// Use Argo's actual evaluator, with x Failed and the delete Failed.
			// Both unparenthesized forms enable b while a remains alive.
			env := map[string]any{"x": map[string]bool{"Succeeded": false, "Failed": true, "Errored": false}, "da": map[string]bool{"Succeeded": false}}
			enabled, err := argoexpr.EvalBool(expression, env)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if enabled {
				want = 2
			}
			checkVMISoundnessDemand(t, wf, binding, policy, want)
		})
	}
}

func TestRunVMISkippedProducerInputDefaultsSoundness(t *testing.T) {
	for _, graph := range []string{"dag-bare", "dag-five-state", "dag-succeeded-sentinel", "steps", "steps-unconditional-sentinel", "dag-through-skipped"} {
		for _, defaultKind := range []string{"default", "value"} {
			t.Run(graph+"/"+defaultKind, func(t *testing.T) {
				wf, binding, policy := vmiSoundnessWorkflow(t)
				selector := wfv1.Template{Name: "selector", Container: &corev1.Container{Image: "busybox:1.37@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Command: []string{"true"}}, Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "partition", ValueFrom: &wfv1.ValueFrom{Path: "/tmp/partition"}}}}}
				wf.Spec.Templates = append(wf.Spec.Templates, selector)
				for i, name := range []string{"part0", "part1"} {
					literal := []string{"0", "1"}[i]
					input := wfv1.Parameter{Name: "partition"}
					if defaultKind == "default" {
						input.Default = wfv1.AnyStringPtr(literal)
					} else {
						input.Value = wfv1.AnyStringPtr(literal)
					}
					part := wfv1.Template{Name: name, Inputs: wfv1.Inputs{Parameters: []wfv1.Parameter{input}}, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "create", Template: []string{"create-a", "create-b"}[i], When: "{{=inputs.parameters['partition'] == '" + literal + "'}}"}}}}
					// Pin Argo v4.0.8's sentinel -> consumer default behavior.
					args := wfv1.Arguments{Parameters: []wfv1.Parameter{{Name: "partition", Value: wfv1.AnyStringPtr(common.AbsentOptionalArgumentValue)}}}
					processed, err := common.ProcessArgs(argologging.TestContext(t.Context()), &part, &args, common.Parameters{}, common.Parameters{}, false, "ci", nil)
					if err != nil || processed.Inputs.Parameters[0].Value.String() != literal {
						t.Fatalf("Argo did not select consumer fallback %q: %v", literal, err)
					}
					wf.Spec.Templates = append(wf.Spec.Templates, part)
				}
				when := "{{workflow.parameters.oberth-trigger}} == schedule"
				if graph == "dag-succeeded-sentinel" || graph == "steps-unconditional-sentinel" {
					// Outputs are untrusted strings even after success. Emitting
					// the sentinel literally has the same ProcessArgs behavior.
					when = ""
					runVMITemplate(wf, "selector").Container.Command = []string{"sh", "-c", "printf '%s' '" + common.AbsentOptionalArgumentValue + "' > /tmp/partition"}
				}
				main := wfv1.Template{Name: "main"}
				if graph == "steps" || graph == "steps-unconditional-sentinel" {
					main.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "select", Template: "selector", When: when}}}, {Steps: []wfv1.WorkflowStep{
						{Name: "p0", Template: "part0", Arguments: vmiPartitionArgs("steps")},
						{Name: "p1", Template: "part1", Arguments: vmiPartitionArgs("steps")},
					}}}
				} else {
					depends := "select"
					switch graph {
					case "dag-five-state":
						depends = "(select.Succeeded || select.Failed || select.Errored || select.Skipped || select.Omitted)"
					case "dag-succeeded-sentinel":
						depends = "select.Succeeded"
					}
					if graph == "dag-through-skipped" {
						depends = "bridge.Succeeded"
					}
					main.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
						{Name: "select", Template: "selector", When: when},
						{Name: "p0", Template: "part0", Depends: depends, Arguments: vmiPartitionArgs("tasks")},
						{Name: "p1", Template: "part1", Depends: depends, Arguments: vmiPartitionArgs("tasks")},
					}}
					if graph == "dag-through-skipped" {
						main.DAG.Tasks = append(main.DAG.Tasks, wfv1.DAGTask{Name: "bridge", Template: "neutral", Depends: "select"})
					}
				}
				wf.Spec.Templates = append(wf.Spec.Templates, main)
				checkVMISoundnessDemand(t, wf, binding, policy, 2)
			})
		}
	}
}

func vmiPartitionArgs(kind string) wfv1.Arguments {
	return wfv1.Arguments{Parameters: []wfv1.Parameter{{Name: "partition", Value: wfv1.AnyStringPtr("{{" + kind + ".select.outputs.parameters.partition}}")}}}
}

func TestRunVMINonSuccessDeleteEdgeCannotRelease(t *testing.T) {
	for _, depends := range []string{
		"da.Succeeded",
		"(da.Succeeded || da.Failed || da.Errored)",
		"(da.Succeeded || da.Failed || da.Errored || da.Skipped || da.Omitted)",
	} {
		t.Run(depends, func(t *testing.T) {
			wf, binding, policy := vmiSoundnessWorkflow(t)
			wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{FailFast: ptr.To(false), Tasks: []wfv1.DAGTask{
				{Name: "a", Template: "create-a"},
				{Name: "da", Template: "delete-a", Depends: "a.Succeeded"},
				{Name: "b", Template: "create-b", Depends: depends},
			}}})
			want := 2
			if depends == "da.Succeeded" {
				want = 1
			}
			// Only two creates: no transitivity check can accidentally mask
			// a mutant that gives a non-success arc deletion credit.
			checkVMISoundnessDemand(t, wf, binding, policy, want)
		})
	}
}

func TestRunVMIDifferentScalarEqualitiesUnderOR(t *testing.T) {
	wf, binding, policy := vmiSoundnessWorkflow(t)
	source, _, _, _ := runVMIFixture(t)
	original, err := Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	wf.Spec.Templates = append(wf.Spec.Templates, *runVMITemplate(original, "selector").DeepCopy())
	zero := "{{=tasks['select'].outputs.parameters['partition'] == '0'}}"
	one := "{{=tasks['select'].outputs.parameters['partition'] == '1'}}"
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
		{Name: "select", Template: "selector"},
		{Name: "a", Template: "create-a", Depends: "select.Succeeded", When: zero + " || " + one},
		{Name: "b", Template: "create-b", Depends: "select.Succeeded", When: one},
	}}})
	// For the same produced value '1', a and b both run. Preserving only
	// the first equality of a's OR would falsely declare them exclusive.
	checkVMISoundnessDemand(t, wf, binding, policy, 2)
}

func TestRunVMISuccessfulProducerWithoutConsumerFallback(t *testing.T) {
	for _, graph := range []string{"dag", "steps"} {
		t.Run(graph, func(t *testing.T) {
			wf, binding, policy := vmiSoundnessWorkflow(t)
			source, _, _, _ := runVMIFixture(t)
			original, err := Decode(source)
			if err != nil {
				t.Fatal(err)
			}
			wf.Spec.Templates = append(wf.Spec.Templates, *runVMITemplate(original, "selector").DeepCopy())
			for i, name := range []string{"part0", "part1"} {
				wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: name, Inputs: wfv1.Inputs{Parameters: []wfv1.Parameter{{Name: "partition"}}}, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "create", Template: []string{"create-a", "create-b"}[i], When: "{{=inputs.parameters['partition'] == '" + []string{"0", "1"}[i] + "'}}"}}}})
			}
			main := wfv1.Template{Name: "main"}
			if graph == "dag" {
				main.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
					{Name: "select", Template: "selector"},
					{Name: "p0", Template: "part0", Depends: "select.Succeeded", Arguments: vmiPartitionArgs("tasks")},
					{Name: "p1", Template: "part1", Depends: "select.Succeeded", Arguments: vmiPartitionArgs("tasks")},
				}}
			} else {
				main.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "select", Template: "selector"}}}, {Steps: []wfv1.WorkflowStep{
					{Name: "p0", Template: "part0", Arguments: vmiPartitionArgs("steps")},
					{Name: "p1", Template: "part1", Arguments: vmiPartitionArgs("steps")},
				}}}
			}
			wf.Spec.Templates = append(wf.Spec.Templates, main)
			checkVMISoundnessDemand(t, wf, binding, policy, 1)
		})
	}
}
