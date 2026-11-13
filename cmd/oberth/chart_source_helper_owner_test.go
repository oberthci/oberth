package main

import "testing"

func TestSourceHelperOwnerPatchUsesExistingServerRBAC(t *testing.T) {
	docs := renderArgoRBAC(t, "-s", "templates/rbac-argo-identity-retry.yaml")
	role := findChartRBACDoc(t, docs, "Role", "oberth-argo-identity-retry")
	want := []map[string]any{{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"patch"}}}
	if canonicalRules(t, role.Rules) != canonicalRules(t, want) {
		t.Fatal(role.Rules)
	}
	binding := findChartRBACDoc(t, docs, "RoleBinding", "oberth-argo-identity-retry")
	frozen := findChartRBACDoc(t, docs, "RoleBinding", "oberth-argo")
	if canonicalRules(t, binding.Subjects) != canonicalRules(t, frozen.Subjects) || role.Metadata.Namespace != frozen.Metadata.Namespace {
		t.Fatal("Pod patch is not bound to the same server/namespace", binding)
	}
}
