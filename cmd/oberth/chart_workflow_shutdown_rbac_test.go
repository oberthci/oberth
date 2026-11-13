package main

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestServerWorkflowShutdownRBACCannotDeleteWorkflows(t *testing.T) {
	output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+testImageDigest, "-s", "templates/rbac-argo.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render RBAC: %v %s", err, output)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	workflowRules := 0
	for {
		var document struct {
			Kind  string
			Rules []struct{ APIGroups, Resources, Verbs []string }
		}
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if document.Kind != "Role" {
			continue
		}
		for _, rule := range document.Rules {
			group, resource := false, false
			for _, g := range rule.APIGroups {
				group = group || g == "argoproj.io" || g == "*"
			}
			for _, r := range rule.Resources {
				resource = resource || r == "workflows" || r == "*"
			}
			if !group || !resource {
				continue
			}
			workflowRules++
			for _, verb := range rule.Verbs {
				if verb == "delete" || verb == "deletecollection" || verb == "*" {
					t.Fatalf("server shutdown Role can delete Workflows: %+v", rule)
				}
			}
		}
	}
	if workflowRules == 0 {
		t.Fatal("missing Workflow RBAC; test examined no permissions")
	}
}
