package main

import "testing"

func TestNodeChartGrantsOnlyBootReadsToServer(t *testing.T) {
	for _, namespace := range []string{"first", "second"} {
		t.Run(namespace, func(t *testing.T) {
			docs := renderArgoRBAC(t, "-s", "templates/rbac-node-boots.yaml", "--namespace", namespace)
			name := "oberth-" + namespace + "-node-boots"
			role := findChartRBACDoc(t, docs, "ClusterRole", name)
			want := []map[string]any{{"apiGroups": []any{""}, "resources": []any{"nodes"}, "verbs": []any{"get"}}}
			if canonicalRules(t, role.Rules) != canonicalRules(t, want) {
				t.Fatalf("node read permission broadened: %+v", role.Rules)
			}
			binding := findChartRBACDoc(t, docs, "ClusterRoleBinding", name)
			if binding.RoleRef["kind"] != "ClusterRole" || binding.RoleRef["name"] != name || len(binding.Subjects) != 1 || binding.Subjects[0]["name"] != "oberth" || binding.Subjects[0]["kind"] != "ServiceAccount" || binding.Subjects[0]["namespace"] != namespace {
				t.Fatalf("node reader binding: %+v", binding)
			}
		})
	}
}
