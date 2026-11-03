package main

import (
	"bytes"
	"os/exec"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestChartCacheInitializerCapabilitiesBounded(t *testing.T) {
	t.Parallel()
	out, err := exec.CommandContext(t.Context(), "helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+testImageDigest,
		"-s", "templates/deployment.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render Deployment: %v\n%s", err, out)
	}
	var deployment appsv1.Deployment
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096).Decode(&deployment); err != nil {
		t.Fatal(err)
	}
	init := deployment.Spec.Template.Spec.InitContainers
	if len(init) != 1 || init[0].Name != "prepare-caches" {
		t.Fatalf("unexpected cache initializers: %+v", init)
	}
	sc := init[0].SecurityContext
	if sc == nil || sc.Capabilities == nil ||
		!slices.Equal(sc.Capabilities.Add, []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER"}) ||
		!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Fatalf("cache initializer capabilities expanded: %+v", sc)
	}
	if sc.Privileged != nil && *sc.Privileged || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Fatalf("cache initializer confinement weakened: %+v", sc)
	}
}
