package main

import (
	"os/exec"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestChartReadinessToleratesHTTPSDelays(t *testing.T) {
	output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set",
		"image.ref=example.invalid/oberth@sha256:"+strings.Repeat("1", 64),
		"--show-only", "templates/deployment.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render deployment: %v\n%s", err, output)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(output, &deployment); err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	probe := container.ReadinessProbe
	if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/readyz" || probe.HTTPGet.Scheme != "HTTPS" ||
		probe.TimeoutSeconds != 5 || probe.PeriodSeconds != 5 || probe.FailureThreshold != 6 {
		t.Fatalf("unexpected readiness timing: %+v", probe)
	}
	if container.StartupProbe.HTTPGet.Path != "/healthz" || container.StartupProbe.FailureThreshold != 60 || container.LivenessProbe.HTTPGet.Path != "/livez" || container.LivenessProbe.TimeoutSeconds != 15 {
		t.Fatal("readiness timing changed the independent startup/liveness policy")
	}
}
