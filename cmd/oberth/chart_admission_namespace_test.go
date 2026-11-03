package main

import (
	"bytes"
	"os/exec"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestChartAdmissionConfigMapNamespace(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "helm", "template", "product", "../../charts/oberth",
		"--namespace", "server-custom", "--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"--set", "admission.runtimeCapacity.enabled=true", "-s", "templates/admission-configmap.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render admission ConfigMap: %v\n%s", err, out)
	}
	var cm corev1.ConfigMap
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096).Decode(&cm); err != nil {
		t.Fatal(err)
	}
	if cm.Namespace != "server-custom" {
		t.Fatalf("admission ConfigMap namespace = %q, want server-custom", cm.Namespace)
	}
}
