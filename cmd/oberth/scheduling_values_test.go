package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func renderSchedulingChart(t *testing.T, sets ...string) ([]string, error) {
	t.Helper()
	arguments := []string{"template", "oberth", "../../charts/oberth", "--show-only", "templates/deployment.yaml", "--set", "image.ref=example.invalid/oberth@sha256:" + strings.Repeat("1", 64)}
	for _, set := range sets {
		arguments = append(arguments, "--set", set)
	}
	output, err := exec.Command("helm", arguments...).CombinedOutput()
	if err != nil {
		return nil, err
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(output, &deployment); err != nil {
		t.Fatal(err)
	}
	return deployment.Spec.Template.Spec.Containers[0].Args[1:], nil
}

func TestRetiredSchedulingFlagsAndValuesAreRejected(t *testing.T) {
	for _, flag := range []string{"--admission-stall-timeout=1m", "--admission-step-startup-allowance-percent=0", "--admission-step-startup-allowance-min-seconds=0", "--admission-step-startup-allowance-max-seconds=0", "--argo-workflow-timeout=1h", "--argo-default-workflow-timeout=1h", "--argo-repo-workflow-timeout=repo=1h", "--serialize-releases=true"} {
		if _, err := parseServeOptions([]string{flag}, io.Discard); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("%s: %v", flag, err)
		}
	}
	for _, value := range []string{"admission.stallTimeout=1m", "argo.workflowTimeout=1h", "argo.defaultWorkflowTimeout=1h", "argo.repoWorkflowTimeouts.repo=1h", "releases.serialize=true"} {
		if _, err := renderSchedulingChart(t, value); err == nil {
			t.Fatal(value)
		}
	}
	args, err := renderSchedulingChart(t, "schedules.minInterval=1h", "schedules.maxEntries=3")
	if err != nil {
		t.Fatal(err)
	}
	options, err := parseServeOptions(args, io.Discard)
	if err != nil || options.scheduleMinInterval != time.Hour || options.scheduleMaxEntries != 3 {
		t.Fatal(options, err)
	}
}
