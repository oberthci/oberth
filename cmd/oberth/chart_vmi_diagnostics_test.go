package main

import "testing"

func TestChartVMIDiagnosticsUseSeparateEventReadRole(t *testing.T) {
	docs := renderArgoRBAC(t, "--namespace", "server", "--set", "argo.namespace=pipelines")
	role := findChartRBACDoc(t, docs, "Role", "oberth-argo-vmi-diagnostics")
	want := []map[string]any{{"apiGroups": []any{""}, "resources": []any{"events"}, "verbs": []any{"list"}}}
	if role.Metadata.Namespace != "pipelines" || canonicalRules(t, role.Rules) != canonicalRules(t, want) {
		t.Fatalf("unexpected diagnostic permissions: %+v", role)
	}
	binding := findChartRBACDoc(t, docs, "RoleBinding", role.Metadata.Name)
	if binding.Metadata.Namespace != "pipelines" || binding.RoleRef["kind"] != "Role" || binding.RoleRef["name"] != role.Metadata.Name || len(binding.Subjects) != 1 || binding.Subjects[0]["kind"] != "ServiceAccount" || binding.Subjects[0]["name"] != "oberth" || binding.Subjects[0]["namespace"] != "server" {
		t.Fatalf("unexpected diagnostic binding: %+v", binding)
	}
}
