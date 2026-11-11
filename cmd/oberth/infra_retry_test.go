package main

import (
	"io"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestInfrastructureRetryBudgetSetting(t *testing.T) {
	for _, budget := range []int{0, 1, 5} {
		options, err := parseServeOptions([]string{"--argo-namespace=argo-pipelines", "--auto-retry-infrastructure-budget=" + strconv.Itoa(budget)}, io.Discard)
		if err != nil || options.autoRetryBudget != budget {
			t.Fatalf("budget %d: %d %v", budget, options.autoRetryBudget, err)
		}
	}
	for _, budget := range []string{"-1", "6"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo-pipelines", "--auto-retry-infrastructure-budget=" + budget}, io.Discard); err == nil {
			t.Fatalf("budget %s accepted", budget)
		}
	}
}

func TestInfrastructureRetryBudgetDefaultTwo(t *testing.T) {
	options, err := parseServeOptions([]string{"--argo-namespace=argo-pipelines"}, io.Discard)
	if err != nil || options.autoRetryBudget != 2 {
		t.Fatalf("default budget %d: %v", options.autoRetryBudget, err)
	}
}

func TestChartInfrastructureRetryBudget(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm unavailable")
	}
	for value, want := range map[string]string{"": "2", "0": "0", "3": "3"} {
		args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + goProxyDigest}
		if value != "" {
			args = append(args, "--set", "autoRetryInfrastructureBudget="+value)
		}
		out, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("chart: %v %s", err, out)
		}
		if !strings.Contains(string(out), "--auto-retry-infrastructure-budget="+want+"\n") {
			t.Fatalf("budget %q not rendered as %s", value, want)
		}
	}
	args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + goProxyDigest, "--set", "autoRetryInfrastructureBudget=9"}
	if out, err := exec.Command("helm", args...).CombinedOutput(); err == nil {
		t.Fatalf("budget 9 rendered: %s", out)
	}
}
