package integration_test

import (
	"os/exec"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestChartPassesUplinkTokenLifetimePolicyToServer(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required to render the chart")
	}
	for _, custom := range []bool{false, true} {
		args := []string{"template", "oberth", "../../charts/oberth", "--show-only", "templates/deployment.yaml"}
		defaultTTL, maxTTL := "2160h", "8760h"
		if custom {
			defaultTTL, maxTTL = "720h", "1440h"
			args = append(args, "--set-string", "uplinkTokens.defaultTTL="+defaultTTL, "--set-string", "uplinkTokens.maxTTL="+maxTTL)
		}
		output, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("render: %v %s", err, output)
		}
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal(output, &deployment); err != nil {
			t.Fatal(err)
		}
		passed := false
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name != "oberth" {
				continue
			}
			passed = slices.Contains(container.Args, "--uplink-token-default-ttl="+defaultTTL) && slices.Contains(container.Args, "--uplink-token-max-ttl="+maxTTL)
		}
		if !passed {
			t.Fatalf("chart did not pass configured TTLs: %s, %s", defaultTTL, maxTTL)
		}
	}
}
