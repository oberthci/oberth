package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

// The `oberth-argo` Role's rules list is frozen at its v0.17.9 shape: older
// installs carry a separate field manager on `.rules` (kubectl-patch from an
// emergency repair, kubectl-client-side-apply from adoption), and Helm 4's
// server-side apply conflicts on any value that differs from the one that
// manager owns, half-applying the upgrade. v0.17.10 (2c532e0) prepended two
// rules and broke that contract on the first upgrade of tuxbox (issue #813).
// New permissions must be added as a new Role bound to the same
// ServiceAccount. These tests pin both halves.

type chartRBACDoc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Rules    []map[string]any `json:"rules"`
	Subjects []map[string]any `json:"subjects"`
	RoleRef  map[string]any   `json:"roleRef"`
}

func renderArgoRBAC(t *testing.T, extra ...string) []chartRBACDoc {
	t.Helper()
	args := []string{"template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@" + testImageDigest,
		"-s", "templates/rbac-argo.yaml"}
	args = append(args, extra...)
	out, err := exec.Command("helm", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("render chart: %v\n%s", err, exitErr.Stderr)
		}
		t.Fatalf("render chart: %v", err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	var docs []chartRBACDoc
	for {
		var doc chartRBACDoc
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode rendered RBAC: %v", err)
		}
		if doc.Kind != "" {
			docs = append(docs, doc)
		}
	}
	return docs
}

func findChartRBACDoc(t *testing.T, docs []chartRBACDoc, kind, name string) chartRBACDoc {
	t.Helper()
	for _, doc := range docs {
		if doc.Kind == kind && doc.Metadata.Name == name {
			return doc
		}
	}
	t.Fatalf("%s %q not rendered", kind, name)
	return chartRBACDoc{}
}

// canonicalRules renders a rules list as JSON with sorted keys so two lists
// compare structurally (order of rules preserved, order of keys normalized).
func canonicalRules(t *testing.T, rules []map[string]any) string {
	t.Helper()
	encoded, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestChartArgoRoleRulesFrozenAtV0179(t *testing.T) {
	t.Parallel()
	variants := map[string][]string{
		"default": nil,
		"nonroot": {"--set", "argo.controllerProfile=nonroot-static-v1"},
	}
	for variant, extra := range variants {
		docs := renderArgoRBAC(t, extra...)
		role := findChartRBACDoc(t, docs, "Role", "oberth-argo")
		fixture, err := os.ReadFile(filepath.Join("testdata", "argo-role-rules-v0.17.9-"+variant+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var want []map[string]any
		if err := json.Unmarshal(fixture, &want); err != nil {
			t.Fatal(err)
		}
		if got, wantCanonical := canonicalRules(t, role.Rules), canonicalRules(t, want); got != wantCanonical {
			t.Fatalf("%s: Role oberth-argo rules differ from the frozen v0.17.9 shape (issue #813).\n"+
				"Add new permissions in a separate Role bound to the same ServiceAccount.\n--- got ---\n%s\n--- want ---\n%s",
				variant, got, wantCanonical)
		}
	}
}

func TestChartArgoExecutorRulesLiveInTheirOwnRole(t *testing.T) {
	t.Parallel()
	docs := renderArgoRBAC(t)
	role := findChartRBACDoc(t, docs, "Role", "oberth-argo-executor-tokens")
	want := []map[string]any{
		{"apiGroups": []any{""}, "resources": []any{"serviceaccounts/token"}, "resourceNames": []any{"oberth-argo-executor"}, "verbs": []any{"create"}},
		{"apiGroups": []any{""}, "resources": []any{"configmaps"}, "resourceNames": []any{"kube-root-ca.crt"}, "verbs": []any{"get"}},
	}
	if got, wantCanonical := canonicalRules(t, role.Rules), canonicalRules(t, want); got != wantCanonical {
		t.Fatalf("executor Role rules:\n--- got ---\n%s\n--- want ---\n%s", got, wantCanonical)
	}
	if role.Metadata.Namespace != "oberth-argo" {
		t.Fatalf("executor Role must live in the pipeline namespace, got %q", role.Metadata.Namespace)
	}

	// Bound to exactly the ServiceAccount the frozen Role is bound to.
	frozenBinding := findChartRBACDoc(t, docs, "RoleBinding", "oberth-argo")
	binding := findChartRBACDoc(t, docs, "RoleBinding", "oberth-argo-executor-tokens")
	if binding.RoleRef["name"] != "oberth-argo-executor-tokens" || binding.RoleRef["kind"] != "Role" {
		t.Fatalf("executor RoleBinding roleRef: %v", binding.RoleRef)
	}
	if got, wantSubjects := canonicalRules(t, binding.Subjects), canonicalRules(t, frozenBinding.Subjects); got != wantSubjects {
		t.Fatalf("executor RoleBinding subjects differ from the frozen Role's binding:\n%s\nvs\n%s", got, wantSubjects)
	}

	// The executor rules appear nowhere else in the frozen Role.
	frozen := findChartRBACDoc(t, docs, "Role", "oberth-argo")
	for _, rule := range frozen.Rules {
		for _, name := range toStrings(rule["resourceNames"]) {
			if name == "oberth-argo-executor" || name == "kube-root-ca.crt" {
				t.Fatalf("frozen Role oberth-argo carries an executor rule: %v", rule)
			}
		}
	}
}

func toStrings(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
