package argoworkflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Knetic/govaluate"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	argotemplate "github.com/argoproj/argo-workflows/v4/util/template"
)

// Mirror the pinned Argo v4.0.8 shouldExecute oracle: substitute first, convert
// VARIABLE tokens to strings, then let govaluate evaluate its actual precedence.
// This is test-only. Production recognizes a closed grammar without evaluation.
func reviewArgoWhen(t *testing.T, guard string, outputs map[string]string) (bool, error) {
	t.Helper()
	body, err := json.Marshal(guard)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"workflow.parameters.oberth-trigger": "branch"}
	for name, value := range outputs {
		values[name] = value
	}
	replaced, err := argotemplate.Replace(argologging.TestContext(t.Context()), string(body), values, false)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(replaced), &guard); err != nil {
		t.Fatal(err)
	}
	if guard == "" {
		return true, nil
	}
	expression, err := govaluate.NewEvaluableExpression(guard)
	if err != nil {
		return false, err
	}
	tokens := expression.Tokens()
	for i := range tokens {
		if tokens[i].Kind == govaluate.VARIABLE {
			tokens[i].Kind = govaluate.STRING
		}
	}
	expression, err = govaluate.NewEvaluableExpressionFromTokens(tokens)
	if err != nil {
		return false, err
	}
	result, err := expression.Evaluate(nil)
	if err != nil {
		return false, err
	}
	boolean, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("Argo when returned a non-boolean: %v", result)
	}
	return boolean, nil
}

func TestReviewWhenActualArgoPrecedence(t *testing.T) {
	for _, tc := range []struct {
		guard   string
		outputs map[string]string
		want    bool
	}{
		{guard: reviewTernaryFalse, want: true},
		{guard: reviewTernaryTrue, want: false},
		{guard: "{{workflow.parameters.oberth-trigger}} == schedule && {{tasks.sel.outputs.result}}", outputs: map[string]string{"tasks.sel.outputs.result": "true ? true : true"}, want: true},
		{guard: "{{workflow.parameters.oberth-trigger}} == branch || {{steps.sel.outputs.result}}", outputs: map[string]string{"steps.sel.outputs.result": "true ? false : false"}, want: false},
	} {
		t.Run(tc.guard, func(t *testing.T) {
			got, err := reviewArgoWhen(t, tc.guard, tc.outputs)
			if err != nil || got != tc.want {
				t.Fatalf("pinned Argo oracle: %v %v, want %v", got, err, tc.want)
			}
		})
	}
}

func reviewWhenWorkflow(t *testing.T, graph, position, guard string) (*wfv1.Workflow, RunSourceBinding, Policy) {
	t.Helper()
	wf, binding, policy := vmiSoundnessWorkflow(t)
	main := wfv1.Template{Name: "main"}
	if graph == "steps" {
		main.Steps = []wfv1.ParallelSteps{
			{Steps: []wfv1.WorkflowStep{{Name: "a", Template: "create-a"}}},
			{Steps: []wfv1.WorkflowStep{{Name: "da", Template: "delete-a"}}},
			{Steps: []wfv1.WorkflowStep{{Name: "b", Template: "create-b"}}},
		}
		if position == "create" {
			main.Steps[2].Steps[0].When = guard
			main.Steps = append(main.Steps[:1], main.Steps[2:]...)
		} else {
			main.Steps[1].Steps[0].When = guard
		}
	} else {
		main.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{
			{Name: "a", Template: "create-a"},
			{Name: "da", Template: "delete-a"},
			{Name: "b", Template: "create-b"},
		}}
		if graph == "legacy" {
			main.DAG.Tasks[1].Dependencies = []string{"a"}
			main.DAG.Tasks[2].Dependencies = []string{"da"}
		} else {
			main.DAG.Tasks[1].Depends = "a.Succeeded"
			main.DAG.Tasks[2].Depends = "da"
		}
		if position == "create" {
			main.DAG.Tasks[2].When = guard
			main.DAG.Tasks[2].Depends = ""
			main.DAG.Tasks[2].Dependencies = nil
			main.DAG.Tasks = append(main.DAG.Tasks[:1], main.DAG.Tasks[2:]...)
		} else {
			main.DAG.Tasks[1].When = guard
		}
	}
	wf.Spec.Templates = append(wf.Spec.Templates, main)
	return wf, binding, policy
}

func TestReviewUnparsedWhenAlwaysUsesSum(t *testing.T) {
	for _, graph := range []string{"legacy", "bare", "steps"} {
		for _, position := range []string{"create", "delete"} {
			ref := "{{tasks.sel.outputs.result}}"
			if graph == "steps" {
				ref = "{{steps.sel.outputs.result}}"
			}
			for _, suffix := range []string{
				"true ? true : true", "true ? false : false", "true ?? false", "[true]", `\true`, "some[0]", "true,false", "", "(" + ref + ")", ref,
			} {
				t.Run(graph+"/"+position+"/"+suffix, func(t *testing.T) {
					prefix := "{{workflow.parameters.oberth-trigger}} == schedule && "
					if position == "delete" {
						prefix = "{{workflow.parameters.oberth-trigger}} == branch || "
					}
					guard := prefix + suffix
					wf, binding, policy := reviewWhenWorkflow(t, graph, position, guard)
					if strings.Contains(suffix, ".outputs.") {
						selector := runVMITemplate(wf, "neutral").DeepCopy()
						selector.Name = "raw-selector"
						output := "true ? true : true"
						if position == "delete" {
							output = "true ? false : false"
						}
						selector.Container.Command = []string{"printf", "%s", output}
						main := runVMITemplate(wf, "main")
						if graph == "steps" {
							main.Steps = append([]wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "sel", Template: selector.Name}}}}, main.Steps...)
						} else {
							target := &main.DAG.Tasks[1]
							if graph == "legacy" {
								target.Dependencies = append(target.Dependencies, "sel")
							} else if target.Depends == "" {
								target.Depends = "sel.Succeeded"
							} else {
								target.Depends += " && sel.Succeeded"
							}
							main.DAG.Tasks = append(main.DAG.Tasks, wfv1.DAGTask{Name: "sel", Template: selector.Name})
						}
						wf.Spec.Templates = append(wf.Spec.Templates, *selector)
					}
					before, err := json.Marshal(wf)
					if err != nil {
						t.Fatal(err)
					}
					if _, supported := runVMIPeak(wf, binding.Trigger); supported {
						t.Fatal("unparsed operand was folded away instead of falling back")
					}
					checkVMISoundnessDemand(t, wf, binding, policy, 2)
					// Analysis must not normalize or sanitize authored guards.
					after, err := json.Marshal(wf)
					if err != nil || !bytes.Equal(before, after) {
						t.Fatal("analysis changed authored workflow or guard bytes", err)
					}
				})
			}
		}
	}
}

func TestReviewClosedWhenGrammarMatchesArgo(t *testing.T) {
	for _, guard := range []string{
		"", "true", "false", "{{workflow.parameters.oberth-trigger}} == branch",
		"'{{workflow.parameters.oberth-trigger}}' != \"tag\"",
		"({{workflow.parameters.oberth-trigger}} == schedule) || (true && false)",
		"false || {{workflow.parameters.oberth-trigger}} != schedule && true",
		"(false || true) && ({{workflow.parameters.oberth-trigger}} == branch)",
		"true || (false && {{workflow.parameters.oberth-trigger}} == tag)",
	} {
		t.Run(guard, func(t *testing.T) {
			want, err := reviewArgoWhen(t, guard, nil)
			if err != nil {
				t.Fatal(err)
			}
			condition := parseVMICondition(guard, RunTriggerBranch, func(string) (vmiScalar, bool) { return vmiScalar{}, false }, 0)
			if condition.unsafe || (condition.truth == vmiTrue) != want || condition.truth == vmiUnknown {
				t.Fatalf("documented grammar diverged from Argo: %+v, want %v", condition, want)
			}
		})
	}
}

func TestReviewUnparsedLeadingOperandCannotBeFoldedAway(t *testing.T) {
	for _, unknown := range []string{"true ? true : true", "true ?? false", "some[0]", `\true`, ""} {
		for _, tail := range []string{" && false", " || true"} {
			t.Run(unknown+tail, func(t *testing.T) {
				wf, binding, policy := reviewWhenWorkflow(t, "bare", "create", unknown+tail)
				if _, supported := runVMIPeak(wf, binding.Trigger); supported {
					t.Fatal("leading unknown fragment was folded away")
				}
				checkVMISoundnessDemand(t, wf, binding, policy, 2)
			})
		}
	}
}
