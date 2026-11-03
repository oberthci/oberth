package main

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestChartIgnoresRetiredExecutionValues(t *testing.T) {
	args := []string{"template", "product", "../../charts/oberth", "--namespace", "ci", "--set-string", "image.ref=example.invalid/oberth@" + goProxyDigest}
	baseline, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("baseline: %v\n%s", err, baseline)
	}
	args = append(args, "--set", "vm.kvm.enabled=true", "--set-json", `trustedTests={"recipes":[{"obsolete":true}]}`, "--set-string", "argo.vmLauncherServiceAccount=old-launcher", "--set-json", `argo.containerDiskPrefixes=["obsolete.invalid/"]`, "--set", "argo.kubevirtCeiling.maxVMs=99")
	upgraded, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("reused retired values rejected: %v\n%s", err, upgraded)
	}
	if !bytes.Equal(baseline, upgraded) {
		t.Fatal("retired values changed the rendered workload or permissions")
	}
	for _, name := range []string{"kubevirt.io", "argo-vm-launcher", "trusted-test-recipes", "vm-kvm-enabled", "vmi-reservations"} {
		if bytes.Contains(baseline, []byte(name)) {
			t.Errorf("retired execution substrate remains: %s", name)
		}
	}
}
