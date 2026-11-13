package main

import "testing"

func TestSourceHelperOwnerPatchUsesExistingServerRBAC(t *testing.T) {
	docs := renderArgoRBAC(t, "--namespace", "server-system", "--set", "argo.namespace=pipeline-system")
	role := findChartRBACDoc(t, docs, "Role", "oberth-argo")
	var podRules []map[string]any
	for _, rule := range role.Rules {
		resources, _ := rule["resources"].([]any)
		if len(resources) == 1 && resources[0] == "pods" {
			podRules = append(podRules, rule)
		}
	}
	want := []map[string]any{{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"create", "get", "list", "watch", "delete", "patch"}}}
	if canonicalRules(t, podRules) != canonicalRules(t, want) {
		t.Fatal("folded Pod permissions changed", podRules)
	}
	binding := findChartRBACDoc(t, docs, "RoleBinding", "oberth-argo")
	subjects := []map[string]any{{"kind": "ServiceAccount", "name": "oberth", "namespace": "server-system"}}
	ref := []map[string]any{{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "oberth-argo"}}
	if canonicalRules(t, binding.Subjects) != canonicalRules(t, subjects) || canonicalRules(t, []map[string]any{binding.RoleRef}) != canonicalRules(t, ref) || role.Metadata.Namespace != "pipeline-system" || binding.Metadata.Namespace != "pipeline-system" {
		t.Fatal("Pod patch is not bound to the server in the pipeline namespace", binding)
	}
}
