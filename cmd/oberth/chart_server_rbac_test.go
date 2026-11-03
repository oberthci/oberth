package main

import (
	"bytes"
	"os/exec"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestChartServerRoleOnlyConfigMaps(t *testing.T) {
	t.Parallel()
	out, err := exec.CommandContext(t.Context(), "helm", "template", "oberth", "../../charts/oberth",
		"--namespace", "server", "--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"-s", "templates/rbac.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render server RBAC: %v\n%s", err, out)
	}
	var role chartRBACDoc
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096).Decode(&role); err != nil {
		t.Fatal(err)
	}
	if role.Kind != "Role" || role.Metadata.Namespace != "server" {
		t.Fatalf("unexpected role: %+v", role)
	}
	want := []map[string]any{
		{"apiGroups": []string{""}, "resources": []string{"configmaps"}, "verbs": []string{"create", "get", "list", "watch"}},
		{"apiGroups": []string{""}, "resources": []string{"configmaps"}, "resourceNames": []string{"oberth-secret-access"}, "verbs": []string{"update"}},
	}
	if canonicalRules(t, role.Rules) != canonicalRules(t, want) {
		t.Fatalf("server Role must only grant ConfigMap access, got:\n%s", canonicalRules(t, role.Rules))
	}
}
