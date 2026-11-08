package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/argojob"
)

// Every scheduling and placement setting is a Helm value: the step start-up
// allowance is bounded and its defaults equal the previous hard-coded policy.
func TestServeStepStartupAllowanceFlags(t *testing.T) {
	defaults, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.stepStartupAllowance != argojob.DefaultStepStartupAllowance || defaults.stepStartupAllowance != (argojob.StepStartupAllowance{FactorPercent: 50, MinSeconds: 120, MaxSeconds: 600}) {
		t.Fatalf("default allowance changed: %+v", defaults.stepStartupAllowance)
	}
	if defaults.argoDefaultWorkflowTimeout != 30*time.Minute {
		t.Fatalf("default workflow timeout changed: %s", defaults.argoDefaultWorkflowTimeout)
	}
	configured, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-step-startup-allowance-percent=0",
		"--admission-step-startup-allowance-min-seconds=0", "--admission-step-startup-allowance-max-seconds=0", "--argo-default-workflow-timeout=45m"}, io.Discard)
	if err != nil || configured.stepStartupAllowance != (argojob.StepStartupAllowance{}) || configured.argoDefaultWorkflowTimeout != 45*time.Minute {
		t.Fatalf("configured values not parsed: %+v %s %v", configured.stepStartupAllowance, configured.argoDefaultWorkflowTimeout, err)
	}
	for _, flag := range []string{
		"--admission-step-startup-allowance-percent=401", "--admission-step-startup-allowance-percent=-1",
		"--admission-step-startup-allowance-max-seconds=3601", "--admission-step-startup-allowance-min-seconds=601",
		"--admission-step-startup-allowance-min-seconds=-1", "--argo-default-workflow-timeout=0s",
		"--argo-default-workflow-timeout=1500ms",
	} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", flag}, io.Discard); err == nil {
			t.Fatalf("accepted %s", flag)
		}
	}
}

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

// The default chart passes every scheduling knob explicitly and renders the
// same values the binary defaults to, so upgrading changes no behaviour.
func TestChartSchedulingValuesDefaultToBinaryDefaults(t *testing.T) {
	args, err := renderSchedulingChart(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--admission-stall-timeout=", "--admission-step-startup-allowance-percent=", "--admission-step-startup-allowance-min-seconds=",
		"--admission-step-startup-allowance-max-seconds=", "--schedule-min-interval=", "--schedule-max-entries=", "--argo-default-workflow-timeout="} {
		found := false
		for _, arg := range args {
			found = found || strings.HasPrefix(arg, flag)
		}
		if !found {
			t.Fatalf("chart does not set %s: %v", flag, args)
		}
	}
	chart, err := parseServeOptions(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if chart.stepStartupAllowance != binary.stepStartupAllowance || chart.admissionStallTimeout != binary.admissionStallTimeout ||
		chart.scheduleMinInterval != binary.scheduleMinInterval || chart.scheduleMaxEntries != binary.scheduleMaxEntries ||
		chart.argoDefaultWorkflowTimeout != binary.argoDefaultWorkflowTimeout || len(chart.argoRepoWorkflowTimeouts) != 0 || chart.argoExecutionTargetConfig != "" {
		t.Fatalf("chart defaults differ from binary defaults: chart %+v %s %s %d %s, binary %+v %s %s %d %s",
			chart.stepStartupAllowance, chart.admissionStallTimeout, chart.scheduleMinInterval, chart.scheduleMaxEntries, chart.argoDefaultWorkflowTimeout,
			binary.stepStartupAllowance, binary.admissionStallTimeout, binary.scheduleMinInterval, binary.scheduleMaxEntries, binary.argoDefaultWorkflowTimeout)
	}
}

func TestChartSchedulingValuesReachTheServer(t *testing.T) {
	args, err := renderSchedulingChart(t,
		"admission.stallTimeout=7m", "admission.stepStartupAllowance.factorPercent=100",
		"admission.stepStartupAllowance.minSeconds=30", "admission.stepStartupAllowance.maxSeconds=900",
		"schedules.minInterval=1h", "schedules.maxEntries=3", "argo.defaultWorkflowTimeout=45m",
		"argo.repoWorkflowTimeouts.codeberg/example/app=20m")
	if err != nil {
		t.Fatal(err)
	}
	options, err := parseServeOptions(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := parseRepoWorkflowTimeouts(options.argoRepoWorkflowTimeouts)
	if err != nil {
		t.Fatal(err)
	}
	if options.admissionStallTimeout != 7*time.Minute || options.stepStartupAllowance != (argojob.StepStartupAllowance{FactorPercent: 100, MinSeconds: 30, MaxSeconds: 900}) ||
		options.scheduleMinInterval != time.Hour || options.scheduleMaxEntries != 3 || options.argoDefaultWorkflowTimeout != 45*time.Minute ||
		caps["codeberg/example/app"] != 20*time.Minute || len(caps) != 1 {
		t.Fatalf("chart values lost: %+v caps %v", options, caps)
	}

	// The serve parser reads the target policy file, so check the rendered path.
	args, err = renderSchedulingChart(t, "argo.executionTargetConfigMap=target-policy", "argo.executionTargetCredentialsSecret=target-kubeconfig")
	if err != nil || !strings.Contains(strings.Join(args, " "), "--argo-execution-target-config=/etc/oberth/execution-target/config.json") {
		t.Fatalf("execution target policy not passed: %v %v", args, err)
	}

	args, err = renderSchedulingChart(t, "admission.stepStartupAllowance.enabled=false")
	if err != nil {
		t.Fatal(err)
	}
	if options, err = parseServeOptions(args, io.Discard); err != nil || options.stepStartupAllowance != (argojob.StepStartupAllowance{}) {
		t.Fatalf("disabled allowance rendered %+v: %v", options.stepStartupAllowance, err)
	}

	for _, set := range []string{
		"admission.stepStartupAllowance.factorPercent=401", "admission.stepStartupAllowance.maxSeconds=3601",
		"admission.stepStartupAllowance.minSeconds=601", "admission.stepStartupAllowance.extra=1",
		"admission.stallTimeout=soon", "schedules.maxEntries=0", "schedules.minInterval=daily",
		"argo.defaultWorkflowTimeout=forever", "argo.repoWorkflowTimeouts.not-a-repo=1h",
		"argo.executionTargetCredentialsSecret=orphan",
	} {
		if _, err := renderSchedulingChart(t, set); err == nil {
			t.Fatalf("chart accepted %s", set)
		}
	}
}
