package main

import (
	"io"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestInfrastructureRetryAdminSetting(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		options, err := parseServeOptions([]string{"--argo-namespace=argo-pipelines", "--auto-retry-infrastructure=" + strconv.FormatBool(enabled)}, io.Discard)
		if options.autoRetryInfrastructure != enabled {
			t.Fatalf("setting lost: %t", enabled)
		}
		if err != nil {
			t.Fatalf("admin setting %t: %v", enabled, err)
		}
	}
}

func TestInfrastructureRetryAdminDefaultOn(t *testing.T) {
	options, err := parseServeOptions([]string{"--argo-namespace=argo-pipelines"}, io.Discard)
	if err != nil || !options.autoRetryInfrastructure {
		t.Fatalf("default disabled: %v", err)
	}
}

func TestChartInfrastructureRetryDefaultAndDisable(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm unavailable")
	}
	for _, value := range []string{"", "false", "true"} {
		args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + goProxyDigest}
		if value != "" {
			args = append(args, "--set", "autoRetryInfrastructure="+value)
		}
		out, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("chart: %v %s", err, out)
		}
		enabled := value != "false"
		if !strings.Contains(string(out), "--auto-retry-infrastructure="+strconv.FormatBool(enabled)) {
			t.Fatalf("setting %q not rendered", value)
		}
	}
}
