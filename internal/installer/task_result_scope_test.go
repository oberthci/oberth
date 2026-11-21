package installer_test

import (
	"slices"
	"strings"
	"testing"
)

func TestRenderedTaskResultScope(t *testing.T) {
	t.Parallel()
	policy := object(t, rendered(t, nil), "ValidatingAdmissionPolicy", "oberth-task-results")["spec"].(map[string]any)
	programs := compilePolicy(t, policy)
	for _, operation := range []string{"CREATE", "UPDATE"} {
		for _, tc := range []struct {
			name, namespace, label, result string
			want                           bool
		}{
			{"own", "oberth-runs", "run-0001", "run-0001-123", true},
			{"another Pod", "oberth-runs", "run-0001", "run-0001-456", false},
			{"another Workflow", "oberth-runs", "run-0002", "run-0002-456", false},
			{"another namespace", "other", "run-0001", "run-0001-123", false},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				result := map[string]any{"metadata": map[string]any{"name": tc.result, "namespace": tc.namespace, "labels": map[string]any{"workflows.argoproj.io/workflow": tc.label}}}
				input := map[string]any{"request": map[string]any{"namespace": tc.namespace, "operation": operation, "userInfo": map[string]any{"username": "system:serviceaccount:oberth-runs:pipeline", "extra": map[string]any{"authentication.kubernetes.io/pod-name": []any{"run-0001-build-123"}}}}, "object": result, "oldObject": nil, "variables": map[string]any{}}
				if operation == "UPDATE" {
					input["oldObject"] = result
				}
				evaluatePolicyInput(t, programs, input, tc.want)
			})
		}
	}
}

func TestPipelineAccountsHaveOnlyTaskResultBinding(t *testing.T) {
	t.Parallel()
	want := `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"oberth-task-results","namespace":"jobs"},"roleRef":{"apiGroup":"rbac.authorization.k8s.io","kind":"Role","name":"oberth-task-results"},"subjects":[{"apiGroup":"rbac.authorization.k8s.io","kind":"Group","name":"system:serviceaccounts:jobs"}]}`
	for _, object := range rendered(t, map[string]any{"pipeline": map[string]any{"namespace": "jobs"}}) {
		if object["kind"] != "RoleBinding" && object["kind"] != "ClusterRoleBinding" {
			continue
		}
		for _, entry := range object["subjects"].([]any) {
			subject := entry.(map[string]any)
			if !pipelineAccountSubject(subject, "jobs") {
				continue
			}
			if jsonOf(t, object) != want {
				t.Fatal("extra authority granted to pipeline accounts", object)
			}
		}
	}
}

func pipelineAccountSubject(subject map[string]any, namespace string) bool {
	name, _ := subject["name"].(string)
	switch subject["kind"] {
	case "ServiceAccount":
		return subject["namespace"] == namespace
	case "User":
		return strings.HasPrefix(name, "system:serviceaccount:"+namespace+":")
	case "Group":
		return slices.Contains([]string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + namespace}, name)
	default:
		return false
	}
}

func TestPipelineAccountSubjectForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind, name, namespace string
		want                  bool
	}{
		{"ServiceAccount", "pipeline", "jobs", true},
		{"ServiceAccount", "pipeline", "other", false},
		{"User", "system:serviceaccount:jobs:pipeline", "", true},
		{"User", "system:serviceaccount:other:pipeline", "", false},
		{"User", "system:serviceaccount:jobs-other:pipeline", "", false},
		{"User", "system:serviceaccount:jobs", "", false},
		{"User", "ordinary-user", "", false},
		{"Group", "system:authenticated", "", true},
		{"Group", "system:serviceaccounts", "", true},
		{"Group", "system:serviceaccounts:jobs", "", true},
		{"Group", "system:serviceaccounts:jobs-other", "", false},
	} {
		t.Run(tc.kind+"/"+tc.name+"/"+tc.namespace, func(t *testing.T) {
			subject := map[string]any{"kind": tc.kind, "name": tc.name, "namespace": tc.namespace}
			if got := pipelineAccountSubject(subject, "jobs"); got != tc.want {
				t.Fatalf("pipeline subject=%v want %v", got, tc.want)
			}
		})
	}
}
