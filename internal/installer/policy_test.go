package installer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/cel/environment"
)

type expression struct {
	name, text string
	boolean    bool
}

func (e expression) GetName() string       { return e.name }
func (e expression) GetExpression() string { return e.text }
func (e expression) ReturnTypes() []*cel.Type {
	if e.boolean {
		return []*cel.Type{cel.BoolType}
	}
	return []*cel.Type{cel.AnyType}
}

// Covers: REQ-C9-053
func TestOptionalPolicyRequestFieldsContract(t *testing.T) {
	t.Parallel()
	policy := object(t, rendered(t, nil), "ValidatingAdmissionPolicy", "oberth-task-results")["spec"].(map[string]any)
	rule := policy["matchConstraints"].(map[string]any)["resourceRules"].([]any)[0].(map[string]any)
	if jsonOf(t, rule["resources"]) != `["workflowtaskresults"]` || strings.Contains(jsonOf(t, rule["operations"]), "DELETE") {
		t.Fatal("policy matches ordinary Pod updates", rule)
	}
	programs := compilePolicy(t, policy)
	mine := "run-" + strings.Repeat("a", 32)
	other := "run-" + strings.Repeat("b", 32)
	for _, tc := range []policyCase{
		{"own create", "CREATE", mine + "-node", mine, "", true, true},
		{"missing Pod prefix dash", "CREATE", mine + "node", mine, "", true, false},
		{"native Workflow name", "CREATE", "build-main-node", "build-main", "", true, true},
		{"own update without subResource", "UPDATE", mine + "-node", mine, mine, true, true},
		{"other Workflow", "CREATE", other + "-node", mine, "", true, false},
		{"missing extras", "CREATE", mine, mine, "", false, false},
		{"missing labels", "CREATE", mine, "", "", true, false},
		{"relabel update", "UPDATE", mine, mine, other, true, false},
		{"missing old object", "UPDATE", mine, mine, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evaluatePolicyCase(t, programs, tc)
		})
	}
	checkPolicyConditions(t, policy)
}

// TestPolicyGuardsLint checks every selected optional field in the rendered policy.
// The missing-field evaluator above additionally proves the guards short-circuit.
func TestPolicyGuardsLint(t *testing.T) {
	t.Parallel()
	for _, o := range rendered(t, nil) {
		if o["kind"] == "ValidatingAdmissionPolicy" {
			lintPolicy(t, o["spec"].(map[string]any))
		}
	}
	for _, text := range []string{"request.subResource == ''", "oldObject.metadata.labels == {}", "request.userInfo.extra['x'] == []", "object.metadata.labels['x'] == 'a'", "true || request.subResource == ''", "has(request.subResource) || request.subResource == ''", "has(oldObject.metadata) && oldObject.metadata.name == 'x'", "has(r.metadata.labels)", "r.metadata.labels == {}"} {
		if guardedExpression(text) == "" {
			t.Fatal("unguarded expression passed lint", text)
		}
	}
}

type policyPrograms struct {
	variables, checks []admissioncel.CompilationResult
}
type policyCase struct {
	name, operation, pod, label, oldLabel string
	extras, want                          bool
}

func compilePolicy(t *testing.T, policy map[string]any) policyPrograms {
	t.Helper()
	base := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion())
	compiler, err := admissioncel.NewCompositedCompiler(base)
	if err != nil {
		t.Fatal(err)
	}
	var variables []admissioncel.CompilationResult
	for _, item := range policy["variables"].([]any) {
		v := item.(map[string]any)
		result := compiler.CompileAndStoreVariable(expression{name: v["name"].(string), text: v["expression"].(string)}, admissioncel.OptionalVariableDeclarations{}, environment.NewExpressions)
		if result.Error != nil {
			t.Fatal(result.Error)
		}
		variables = append(variables, result)
	}
	var checks []admissioncel.CompilationResult
	for _, item := range policy["validations"].([]any) {
		v := item.(map[string]any)
		r := compiler.CompileCELExpression(expression{text: v["expression"].(string), boolean: true}, admissioncel.OptionalVariableDeclarations{}, environment.NewExpressions)
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		checks = append(checks, r)
	}
	return policyPrograms{variables: variables, checks: checks}
}

func evaluatePolicyCase(t *testing.T, programs policyPrograms, tc policyCase) {
	t.Helper()
	request := map[string]any{"namespace": "oberth-runs", "operation": tc.operation, "userInfo": map[string]any{"username": "system:serviceaccount:oberth-runs:step"}}
	if tc.extras {
		request["userInfo"].(map[string]any)["extra"] = map[string]any{"authentication.kubernetes.io/pod-name": []any{tc.pod}}
	}
	input := map[string]any{"request": request, "object": policyResult(tc.label, tc.pod), "oldObject": nil, "variables": map[string]any{}}
	if tc.oldLabel != "" {
		input["oldObject"] = policyResult(tc.oldLabel, tc.pod)
	}
	evaluatePolicyInput(t, programs, input, tc.want)
}

func evaluatePolicyInput(t *testing.T, programs policyPrograms, input map[string]any, want bool) {
	t.Helper()
	for _, v := range programs.variables {
		got, _, err := v.Program.ContextEval(context.Background(), input)
		if err != nil || types.IsError(got) {
			t.Fatal(got, err)
		}
		input["variables"].(map[string]any)[v.ExpressionAccessor.(expression).name] = got
	}
	for _, v := range programs.checks {
		got, _, err := v.Program.ContextEval(context.Background(), input)
		if err != nil || got != types.Bool(want) {
			t.Fatal(got, err, want)
		}
	}
}

func policyResult(label, pod string) any {
	if label == "" {
		return map[string]any{"metadata": map[string]any{}}
	}
	name := label + "-" + pod[strings.LastIndex(pod, "-")+1:]
	if pod == label {
		name = pod
	}
	return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"workflows.argoproj.io/workflow": label}}}
}

func checkPolicyConditions(t *testing.T, policy map[string]any) {
	t.Helper()
	base := environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion())
	compiler, err := admissioncel.NewCompositedCompiler(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range policy["matchConditions"].([]any) {
		v := item.(map[string]any)
		r := compiler.CompileCELExpression(expression{text: v["expression"].(string), boolean: true}, admissioncel.OptionalVariableDeclarations{}, environment.NewExpressions)
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		for _, tc := range []struct {
			username string
			want     bool
		}{{"system:serviceaccount:oberth-runs:step", true}, {"system:serviceaccount:other:step", false}, {"system:serviceaccount:oberth-runs-other:step", false}} {
			request := map[string]any{"userInfo": map[string]any{"username": tc.username}}
			if got, _, err := r.Program.ContextEval(context.Background(), map[string]any{"request": request}); err != nil || got != types.Bool(tc.want) {
				t.Fatal("pipeline account condition", got, err, tc)
			}
		}
	}
}

func lintPolicy(t *testing.T, policy map[string]any) {
	t.Helper()
	for _, section := range []string{"matchConditions", "variables", "validations"} {
		for _, item := range policy[section].([]any) {
			text := item.(map[string]any)["expression"].(string)
			if err := guardedExpression(text); err != "" {
				t.Fatal(err, text)
			}
		}
	}

}

func TestEveryRenderedPolicyCompiles(t *testing.T) {
	t.Parallel()
	for _, o := range rendered(t, nil) {
		if o["kind"] == "ValidatingAdmissionPolicy" {
			spec := o["spec"].(map[string]any)
			compilePolicy(t, spec)
			checkPolicyConditions(t, spec)
		}
	}
}

func TestNativePolicyRequiresOneBoundPod(t *testing.T) {
	t.Parallel()
	policy := object(t, rendered(t, nil), "ValidatingAdmissionPolicy", "oberth-task-results")["spec"].(map[string]any)
	programs := compilePolicy(t, policy)
	for _, extra := range []map[string]any{nil, {"authentication.kubernetes.io/pod-name": nil}, {"authentication.kubernetes.io/pod-name": []any{}}, {"authentication.kubernetes.io/pod-name": []any{"build-one", "build-two"}}} {
		caller := map[string]any{"username": "system:serviceaccount:oberth-runs:step"}
		if extra != nil {
			caller["extra"] = extra
		}
		input := map[string]any{"request": map[string]any{"namespace": "oberth-runs", "operation": "CREATE", "userInfo": caller}, "object": policyResult("build", "build-one"), "oldObject": nil, "variables": map[string]any{}}
		evaluatePolicyInput(t, programs, input, false)
	}
}
