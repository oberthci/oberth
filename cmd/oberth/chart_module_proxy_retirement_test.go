package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestChartModuleProxyRetirement(t *testing.T) {
	args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + chartTestDigest}
	output, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("render chart: %v\n%s", err, output)
	}
	for _, removed := range []string{"goproxy", "containerPort: 8444"} {
		if strings.Contains(string(output), removed) {
			t.Errorf("retired proxy resource remains: %s", removed)
		}
	}
	for _, value := range []string{"argo.goProxy.enabled=true", "argo.goProxy.port=8444", "argo.goProxy.modulePrefix=go.example.test"} {
		output, err := exec.CommandContext(t.Context(), "helm", append(args, "--set", value)...).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "goProxy") {
			t.Fatalf("retired key accepted: %s: %v\n%s", value, err, output)
		}
	}
}
