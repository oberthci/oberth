package main

import "testing"

func TestNodeBootChartGrantsOnlyNodeGetToServer(t *testing.T) {
	docs := renderArgoRBAC(t, "-s", "templates/rbac-node-boots.yaml")
	role := findChartRBACDoc(t, docs, "ClusterRole", "oberth-node-boots")
	want := []map[string]any{{"apiGroups": []any{""}, "resources": []any{"nodes"}, "verbs": []any{"get"}}}
	if canonicalRules(t, role.Rules) != canonicalRules(t, want) {
		t.Fatalf("node read permission broadened: %+v", role.Rules)
	}
	binding := findChartRBACDoc(t, docs, "ClusterRoleBinding", "oberth-node-boots")
	if binding.RoleRef["kind"] != "ClusterRole" || binding.RoleRef["name"] != "oberth-node-boots" || len(binding.Subjects) != 1 || binding.Subjects[0]["name"] != "oberth" || binding.Subjects[0]["kind"] != "ServiceAccount" {
		t.Fatalf("node reader binding: %+v", binding)
	}
}
