package installer_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/predicates/rules"
	"k8s.io/apiserver/pkg/authentication/user"
	"sigs.k8s.io/yaml"
)

func TestNativePolicyNamespaceBinding(t *testing.T) {
	t.Parallel()
	for _, namespace := range []string{"oberth-runs", "jobs"} {
		objects := rendered(t, map[string]any{"pipeline": map[string]any{"namespace": namespace}})
		got := object(t, objects, "ValidatingAdmissionPolicyBinding", "oberth-task-results")
		want := map[string]any{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding", "metadata": map[string]any{"name": "oberth-task-results"}, "spec": map[string]any{"policyName": "oberth-task-results", "validationActions": []any{"Deny"}, "matchResources": map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": namespace}}}}}
		if jsonOf(t, got) != jsonOf(t, want) {
			t.Fatal("task-result binding differs", got)
		}
		var binding admissionv1.ValidatingAdmissionPolicyBinding
		if err := json.Unmarshal([]byte(jsonOf(t, got)), &binding); err != nil {
			t.Fatal(err)
		}
		selector, err := metav1.LabelSelectorAsSelector(binding.Spec.MatchResources.NamespaceSelector)
		if err != nil || !selector.Matches(labels.Set{"kubernetes.io/metadata.name": namespace}) || selector.Matches(labels.Set{"kubernetes.io/metadata.name": "other"}) {
			t.Fatal("policy is not limited to pipeline namespace", err)
		}
	}
}

// The native resource matcher must bypass ordinary Pod and Pod-status updates.
func TestNativePolicyAdmitsPodUpdates(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	if object(t, objects, "ValidatingAdmissionPolicy", "oberth-task-results")["spec"].(map[string]any)["failurePolicy"] != "Fail" {
		t.Fatal("policy must fail closed")
	}
	var policy admissionv1.ValidatingAdmissionPolicy
	if err := json.Unmarshal([]byte(jsonOf(t, object(t, objects, "ValidatingAdmissionPolicy", "oberth-task-results"))), &policy); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		resource, subresource string
		operation             admission.Operation
		want                  bool
	}{{"pods", "", admission.Update, false}, {"pods", "status", admission.Update, false}, {"workflowtaskresults", "", admission.Create, true}, {"workflowtaskresults", "", admission.Update, true}, {"workflowtaskresults", "", admission.Delete, false}} {
		group, version := "", "v1"
		if tc.resource == "workflowtaskresults" {
			group, version = "argoproj.io", "v1alpha1"
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "run-node", Namespace: "oberth-runs"}}
		attrs := admission.NewAttributesRecord(pod, pod, schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, "oberth-runs", "run-node", schema.GroupVersionResource{Group: group, Version: version, Resource: tc.resource}, tc.subresource, tc.operation, &metav1.UpdateOptions{}, false, &user.DefaultInfo{Name: "system:serviceaccount:oberth-runs:step"})
		matched := false
		for _, rule := range policy.Spec.MatchConstraints.ResourceRules {
			matcher := rules.Matcher{Rule: rule.RuleWithOperations, Attr: attrs}
			matched = matched || matcher.Matches()
		}
		if matched != tc.want {
			t.Fatal("native resource matcher must bypass both Pod UPDATE forms", tc, matched)
		}
	}
}

func TestNativeControllerTokenContract(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	cm := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	var restart map[string]any
	if err := yaml.Unmarshal([]byte(cm["failedPodRestart"].(string)), &restart); err != nil {
		t.Fatal(err)
	}
	if jsonOf(t, restart) != `{"enabled":true,"maxRestarts":1}` {
		t.Fatal("native failed Pod restart contract differs", restart)
	}
	for _, forbidden := range []string{"serviceAccountName", "kubeConfig", "KUBECONFIG", "exec-sa-token", "podSpecPatch"} {
		if strings.Contains(jsonOf(t, cm), forbidden) {
			t.Fatal("controller overrides workflow-owned execution credentials", forbidden)
		}
	}
	for _, object := range objects {
		if object["kind"] == "ServiceAccount" && object["metadata"].(map[string]any)["namespace"] == "oberth-runs" {
			t.Fatal("chart creates an executor account")
		}
		if object["kind"] == "Secret" && object["type"] == "kubernetes.io/service-account-token" {
			t.Fatal("chart creates an executor token Secret")
		}
	}
}

func TestNativeServerVerbsContract(t *testing.T) {
	t.Parallel()
	objects := renderedIn(t, map[string]any{"pipeline": map[string]any{"namespace": "jobs"}}, "frontdoor")
	role := object(t, objects, "Role", "oberth-server")
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{"argoproj.io"}, Resources: []string{"workflows"}, Verbs: []string{"create", "get", "list", "watch", "patch"}},
		{APIGroups: []string{"argoproj.io"}, Resources: []string{"workflowtemplates"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts"}, Verbs: []string{"get", "create"}},
		{APIGroups: []string{"argoproj.io"}, Resources: []string{"cronworkflows"}, Verbs: []string{"get", "list", "create", "update", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"oberth-source"}, Verbs: []string{"get"}},
	}
	var actual rbacv1.Role
	if err := json.Unmarshal([]byte(jsonOf(t, role)), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Namespace != "jobs" || !reflect.DeepEqual(actual.Rules, want) {
		t.Fatal("server pipeline permissions differ", role)
	}
	binding := object(t, objects, "RoleBinding", "oberth-server")
	if jsonOf(t, binding["subjects"]) != `[{"kind":"ServiceAccount","name":"oberth","namespace":"frontdoor"}]` || binding["metadata"].(map[string]any)["namespace"] != "jobs" {
		t.Fatal("server account or binding namespace differs", binding)
	}
	if !strings.Contains(jsonOf(t, object(t, objects, "ClusterRole", "frontdoor-oberth-source-review")), `"resources":["tokenreviews"],"verbs":["create"]`) {
		t.Fatal("source loads cannot authenticate Pod-bound tokens")
	}
}

func TestNativeControllerReadsPipelineAccounts(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.PipelineNamespace = "jobs"
	prepare(t, f)
	objects := rendered(t, f.charts.Previews[0].Values)
	name := "oberth-controller"
	var role rbacv1.Role
	if err := json.Unmarshal([]byte(jsonOf(t, object(t, objects, "Role", name))), &role); err != nil {
		t.Fatal(err)
	}
	if role.Namespace != "jobs" || !allowsAccountRead(role.Rules) {
		t.Fatal("controller cannot read Workflow executor accounts")
	}
	var binding rbacv1.RoleBinding
	if err := json.Unmarshal([]byte(jsonOf(t, object(t, objects, "RoleBinding", name))), &binding); err != nil {
		t.Fatal(err)
	}
	want := []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: "oberth"}}
	if binding.RoleRef.Name != role.Name || !reflect.DeepEqual(binding.Subjects, want) {
		t.Fatal("controller account-read authority is not bound to its own account", binding)
	}
}

func allowsAccountRead(rules []rbacv1.PolicyRule) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, "") && slices.Contains(rule.Resources, "serviceaccounts") && slices.Contains(rule.Verbs, "get") {
			return true
		}
	}
	return false
}
