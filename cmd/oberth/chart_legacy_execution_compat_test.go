package main

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestChartLegacyExecutionCompatibility(t *testing.T) {
	args := []string{"template", "oberth", "../../charts/oberth", "--namespace", "ci", "--set-string", "image.ref=example.invalid/oberth@" + goProxyDigest, "--set", "vm.kvm.enabled=true"}
	rendered, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("chart: %v\n%s", err, rendered)
	}
	for _, required := range []string{"--vm-kvm-enabled", "kubevirt.io", "oberth-argo-vm-launcher"} {
		if bytes.Contains(rendered, []byte(required)) {
			t.Errorf("retired runtime or RBAC rendered: %s", required)
		}
	}
}
