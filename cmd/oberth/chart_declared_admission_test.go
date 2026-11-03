package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestChartIgnoresRetiredAdmissionBudgetsAndKillSwitch(t *testing.T) {
	for _, value := range []string{"admission.declaredResources.disabled=true", "admission.declaredResources.cpuMillis=0", "admission.declaredResources.memoryHeadroomPercent=-1", "admission.declaredResources=null"} {
		out, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+testImageDigest, "--set", value).CombinedOutput()
		if err != nil {
			t.Fatalf("legacy value affected upgrade: %s %v %s", value, err, out)
		}
		text := string(out)
		if strings.Count(text, "kind: PriorityClass") != 3 || strings.Count(text, "value: 1000000") != 3 {
			t.Error("platform recovery priorities must remain enabled")
		}
		for _, flag := range []string{"--disable-declared-admission", "--admission-cpu-millis", "--admission-memory-bytes", "--admission-memory-headroom", "--admission-io-runs"} {
			if strings.Contains(text, flag) {
				t.Fatal("retired global budget rendered", flag)
			}
		}
	}
}

func TestChartIgnoresLegacyResourceCharges(t *testing.T) {
	base := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + testImageDigest}
	for _, value := range []string{"", "admission.legacyResources.L.cpu_milli=4500,admission.legacyResources.L.memory_bytes=10737418240", "admission.legacyResources=null", "admission.legacyResources.L.memory_bytes=-1", "admission.legacyResources.XXL.cpu_milli=1000"} {
		args := append([]string{}, base...)
		if value != "" {
			args = append(args, "--set", value)
		}
		out, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil || strings.Contains(string(out), "--admission-legacy-resources") {
			t.Fatalf("retired chart charges affect deployment: %q %v", value, err)
		}
	}
}
