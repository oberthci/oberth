package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestChartDeclaredAdmissionBudgetsAndSingleKillSwitch(t *testing.T) {
	base := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + testImageDigest}
	for _, custom := range []bool{false, true} {
		args := append([]string{}, base...)
		want := []string{"--disable-declared-admission=false", "--admission-cpu-millis=8000", "--admission-memory-bytes=17179869184", "--admission-io-runs=0"}
		if custom {
			args = append(args, "--set", "admission.declaredResources.disabled=true,admission.declaredResources.cpuMillis=4000,admission.declaredResources.memoryBytes=4294967296,admission.declaredResources.ioRuns=2")
			want = []string{"--disable-declared-admission=true", "--admission-cpu-millis=4000", "--admission-memory-bytes=4294967296", "--admission-io-runs=2"}
		}
		out, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("chart render failed: %v", err)
		}
		if strings.Count(string(out), "kind: PriorityClass") != 4 || strings.Count(string(out), "value: 1000000") != 4 {
			t.Error("platform recovery priorities must be enabled by default")
		}
		for _, flag := range want {
			if !strings.Contains(string(out), flag) {
				t.Errorf("missing %s", flag)
			}
		}
	}
	for _, bad := range []string{"cpuMillis=0", "memoryBytes=0", "ioRuns=-1", "ioRuns=65"} {
		args := append(append([]string{}, base...), "--set", "admission.declaredResources."+bad)
		if err := exec.Command("helm", args...).Run(); err == nil {
			t.Fatalf("invalid administrator setting accepted: %s", bad)
		}
	}
}
