package argojobtest

import (
	"fmt"
	admission "k8s.io/api/admissionregistration/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskResultPolicy is the test oracle for task-result admission.
// Pod-bound names select the native node ID for both Pod naming versions.
// Optional authentication extras, result names and old labels are guarded.
func TaskResultPolicy(namespace string) *admission.ValidatingAdmissionPolicy {
	fail := admission.Fail
	return &admission.ValidatingAdmissionPolicy{ObjectMeta: meta.ObjectMeta{Name: "oberth-task-results"}, Spec: admission.ValidatingAdmissionPolicySpec{
		FailurePolicy: &fail,
		MatchConstraints: &admission.MatchResources{ResourceRules: []admission.NamedRuleWithOperations{{RuleWithOperations: admission.RuleWithOperations{
			Operations: []admission.OperationType{admission.Create, admission.Update}, Rule: admission.Rule{APIGroups: []string{"argoproj.io"}, APIVersions: []string{"v1alpha1"}, Resources: []string{"workflowtaskresults"}},
		}}}},
		MatchConditions: []admission.MatchCondition{{Name: "pipeline-accounts", Expression: fmt.Sprintf("request.userInfo.username.startsWith(%q)", "system:serviceaccount:"+namespace+":")}},
		Variables: []admission.Variable{
			{Name: "pod", Expression: "has(request.userInfo.extra) && 'authentication.kubernetes.io/pod-name' in request.userInfo.extra && dyn(request.userInfo.extra['authentication.kubernetes.io/pod-name']) != null && size(request.userInfo.extra['authentication.kubernetes.io/pod-name']) == 1 ? request.userInfo.extra['authentication.kubernetes.io/pod-name'][0] : ''"},
			{Name: "mine", Expression: "([object] + (request.operation == 'UPDATE' ? [oldObject] : [])).all(r, r != null && has(r.metadata.name) && has(r.metadata.labels) && 'workflows.argoproj.io/workflow' in r.metadata.labels && (variables.pod == r.metadata.labels['workflows.argoproj.io/workflow'] ? r.metadata.name == variables.pod : variables.pod.startsWith(r.metadata.labels['workflows.argoproj.io/workflow'] + '-') && r.metadata.name == r.metadata.labels['workflows.argoproj.io/workflow'] + '-' + variables.pod.split('-')[size(variables.pod.split('-')) - 1]))"},
		},
		Validations: []admission.Validation{{Expression: fmt.Sprintf("has(request.namespace) && request.namespace == %q && variables.pod != '' && variables.mine", namespace), Message: "a step writes only its own Pod's task result"}},
	}}
}
