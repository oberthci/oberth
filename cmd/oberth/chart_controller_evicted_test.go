package main

import (
	"testing"
)

func TestControllerEvictedChartAddsOnlySeparateReplicaSetReadRole(t *testing.T) {
	docs := renderArgoRBAC(t, "--set", "argo.controllerProfile=nonroot-static-v1")
	role := findChartRBACDoc(t, docs, "Role", "oberth-argo-controller-history")
	want := `[{"apiGroups":["apps"],"resources":["replicasets"],"verbs":["get"]}]`
	got := canonicalRules(t, role.Rules)
	// Compare structural JSON through the existing canonicalizer.
	expected := []map[string]any{{"apiGroups": []any{"apps"}, "resources": []any{"replicasets"}, "verbs": []any{"get"}}}
	if got != canonicalRules(t, expected) {
		t.Fatal("broad controller history permission", want, got)
	}
	binding := findChartRBACDoc(t, docs, "RoleBinding", "oberth-argo-controller-history")
	if binding.RoleRef["name"] != "oberth-argo-controller-history" || len(binding.Subjects) != 1 || binding.Subjects[0]["name"] != "oberth" {
		t.Fatal("role not bound to server", binding)
	}
	for _, doc := range renderArgoRBAC(t, "--set", "argo.controllerProfile=") {
		if doc.Metadata.Name == "oberth-argo-controller-history" {
			t.Fatal("disabled profile received permission")
		}
	}
}
