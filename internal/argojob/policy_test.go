package argojob_test

import (
	"testing"

	celgo "github.com/google/cel-go/cel"
	celtypes "github.com/google/cel-go/common/types"
	admissionv1 "k8s.io/api/admission/v1"
	api "k8s.io/api/admissionregistration/v1"
	auth "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/cel/environment"

	"github.com/oberthci/oberth/internal/argojob/argojobtest"
)

// expression supplies the Kubernetes compiler's named-expression contract.
type expression struct{ name, text string }

func (e expression) GetName() string          { return e.name }
func (e expression) GetExpression() string    { return e.text }
func (expression) ReturnTypes() []*celgo.Type { return []*celgo.Type{celgo.AnyType} }

type condition struct{ expression }

func (condition) ReturnTypes() []*celgo.Type { return []*celgo.Type{celgo.BoolType} }
func TestTaskResultPolicy(t *testing.T) {
	t.Parallel()
	policy := argojobtest.TaskResultPolicy("ci")
	check, match := policyEvaluators(t, policy)
	for _, tc := range []struct {
		name, pod, label, old, resource, operation string
		want                                       bool
	}{
		{"own", "run-0001-build-123", "run-0001", "", "workflowtaskresults", "CREATE", true},
		{"other", "run-0002-build-123", "run-0001", "", "workflowtaskresults", "CREATE", false},
		{"scheduled child dash boundary", "cron-parent-12-build-123", "cron-parent-1", "", "workflowtaskresults", "CREATE", false},
		{"no extras", "", "run-0001", "", "workflowtaskresults", "CREATE", false},
		{"no labels", "run-0001-build-123", "", "", "workflowtaskresults", "CREATE", false},
		{"relabel old", "run-0001-build-123", "run-0001", "run-0002", "workflowtaskresults", "UPDATE", false},
		{"relabel new", "run-0001-build-123", "run-0002", "run-0001", "workflowtaskresults", "UPDATE", false},
		{"update own", "run-0001-build-123", "run-0001", "run-0001", "workflowtaskresults", "UPDATE", true},
		{"plain Pod update", "", "", "", "pods", "UPDATE", true},
		{"delete", "", "", "", "workflowtaskresults", "DELETE", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operation := admission.Operation(tc.operation)
			object := resultObject(tc.label)
			object.SetName(tc.label + "-123")
			var old runtime.Object
			if tc.operation == "UPDATE" {
				old = resultObject(tc.old)
				old.(*unstructured.Unstructured).SetName(tc.label + "-123")
			}
			u := &user.DefaultInfo{Name: "system:serviceaccount:ci:step"}
			if tc.pod != "" {
				u.Extra = map[string][]string{"authentication.kubernetes.io/pod-name": {tc.pod}}
			}
			attr := admission.NewAttributesRecord(object, old, schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "WorkflowTaskResult"}, "ci", "result", schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: tc.resource}, "", operation, nil, false, u)
			va := &admission.VersionedAttributes{Attributes: attr, VersionedKind: attr.GetKind(), VersionedObject: object, VersionedOldObject: old}
			// The real request omits empty subResource during conversion; no dynamic map fills it in.
			req := &admissionv1.AdmissionRequest{Operation: admissionv1.Operation(tc.operation), Namespace: "ci", UserInfo: auth.UserInfo{Username: u.Name, Extra: map[string]auth.ExtraValue{}}}
			for k, v := range u.Extra {
				req.UserInfo.Extra[k] = auth.ExtraValue(v)
			}
			got := true
			if policyMatches(policy, tc.resource, api.OperationType(tc.operation)) {
				if evaluate(t, match, va, req) {
					got = evaluate(t, check, va, req)
				}
			}
			if got != tc.want {
				t.Fatalf("accepted=%v want %v", got, tc.want)
			}
		})
	}
}
func resultObject(label string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "WorkflowTaskResult", "metadata": map[string]any{"name": "result"}}}
	if label != "" {
		o.SetLabels(map[string]string{"workflows.argoproj.io/workflow": label})
	}
	return o
}
func policyMatches(p *api.ValidatingAdmissionPolicy, resource string, op api.OperationType) bool {
	for _, r := range p.Spec.MatchConstraints.ResourceRules {
		for _, operation := range r.Operations {
			for _, name := range r.Resources {
				if operation == op && name == resource {
					return true
				}
			}
		}
	}
	return false
}
func evaluate(t *testing.T, f admissioncel.ConditionEvaluator, va *admission.VersionedAttributes, req *admissionv1.AdmissionRequest) bool {
	t.Helper()
	values, _, err := f.ForInput(t.Context(), va, req, admissioncel.OptionalVariableBindings{}, nil, 10000000)
	if err != nil || len(values) != 1 {
		t.Fatalf("evaluate: %v %+v", err, values)
	}
	if values[0].Error != nil {
		t.Fatal(values[0].Error)
	}
	return values[0].EvalResult == celtypes.True
}

func policyEvaluators(t *testing.T, policy *api.ValidatingAdmissionPolicy) (admissioncel.ConditionEvaluator, admissioncel.ConditionEvaluator) {
	t.Helper()
	compiler, err := admissioncel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range policy.Spec.Variables {
		r := compiler.CompileAndStoreVariable(expression{v.Name, v.Expression}, admissioncel.OptionalVariableDeclarations{}, environment.NewExpressions)
		if r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	check := compiler.CompileCondition([]admissioncel.ExpressionAccessor{condition{expression{text: policy.Spec.Validations[0].Expression}}}, admissioncel.OptionalVariableDeclarations{}, environment.NewExpressions)
	match := compiler.CompileCondition([]admissioncel.ExpressionAccessor{condition{expression{text: policy.Spec.MatchConditions[0].Expression}}}, admissioncel.OptionalVariableDeclarations{}, environment.NewExpressions)
	return check, match
}

func TestPolicyOptionalAuthenticationFacts(t *testing.T) {
	t.Parallel()
	policy := argojobtest.TaskResultPolicy("ci")
	check, match := policyEvaluators(t, policy)
	for _, tc := range []struct {
		name, user string
		pods       []string
		want       bool
	}{
		{"one name", "system:serviceaccount:ci:step", []string{"run-a-build-123"}, true},
		{"empty workflow label", "system:serviceaccount:ci:step", nil, false},
		{"empty list", "system:serviceaccount:ci:step", []string{}, false},
		{"two names", "system:serviceaccount:ci:step", []string{"run-a-build-123", "run-a-other-456"}, false},
		{"other namespace", "system:serviceaccount:other:step", nil, true},
		{"controller", "system:serviceaccount:system:controller", nil, true},
		{"dashless prefix", "system:serviceaccount:ci:step", []string{"run-abuild-123"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := resultObject("run-a")
			object.SetName("run-a-123")
			if tc.name == "empty workflow label" {
				object.SetLabels(map[string]string{"workflows.argoproj.io/workflow": ""})
			}
			u := &user.DefaultInfo{Name: tc.user}
			attr := admission.NewAttributesRecord(object, nil, schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "WorkflowTaskResult"}, "ci", "result", schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "workflowtaskresults"}, "", admission.Create, nil, false, u)
			va := &admission.VersionedAttributes{Attributes: attr, VersionedKind: attr.GetKind(), VersionedObject: object}
			req := &admissionv1.AdmissionRequest{Operation: admissionv1.Create, Namespace: "ci", UserInfo: auth.UserInfo{Username: tc.user, Extra: map[string]auth.ExtraValue{"authentication.kubernetes.io/pod-name": auth.ExtraValue(tc.pods)}}}
			got := true
			if evaluate(t, match, va, req) {
				got = evaluate(t, check, va, req)
			}
			if got != tc.want {
				t.Fatal("accepted", got)
			}
		})
	}
}
