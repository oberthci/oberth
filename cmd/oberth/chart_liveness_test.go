package main

import (
	"os/exec"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestChartLivenessUsesConservativeLocalProgressProbe(t *testing.T) {
	output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+testImageDigest, "-s", "templates/deployment.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, output)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(output, &deployment); err != nil {
		t.Fatal(err)
	}
	var server *corev1.Container
	for index := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[index].Name == "oberth" {
			server = &deployment.Spec.Template.Spec.Containers[index]
		}
	}
	if server == nil {
		t.Fatal("missing server container")
	}
	for _, check := range []struct {
		probe *corev1.Probe
		path  string
	}{{server.LivenessProbe, "/livez"}, {server.StartupProbe, "/healthz"}, {server.ReadinessProbe, "/readyz"}} {
		if check.probe == nil || check.probe.HTTPGet == nil || check.probe.HTTPGet.Path != check.path || check.probe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
			t.Fatalf("probe must use HTTPS %s: %+v", check.path, check.probe)
		}
	}
	if server.LivenessProbe.PeriodSeconds < 60 || server.LivenessProbe.FailureThreshold < 3 || server.LivenessProbe.TimeoutSeconds < 10 || server.LivenessProbe.TimeoutSeconds > 15 {
		t.Fatalf("liveness restart budget too short: %+v", server.LivenessProbe)
	}
}
