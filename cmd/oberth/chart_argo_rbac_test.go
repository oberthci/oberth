package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

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
