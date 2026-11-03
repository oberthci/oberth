package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestServePressureHardValveDefaultsAndValidation(t *testing.T) {
	defaults, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil || !defaults.admissionPressure.Enabled || !defaults.admissionPressure.HardOnly || defaults.admissionPressure.HardCPU != 50 || defaults.admissionPressure.HardIO != 40 {
		t.Fatal("bad hard-pressure defaults", defaults.admissionPressure, err)
	}
	for _, value := range []string{"--admission-pressure-hard-cpu=NaN", "--admission-pressure-hard-io=Inf", "--admission-pressure-hard-io=101", "--admission-pressure-hard-cpu=10"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", value}, io.Discard); err == nil {
			t.Fatal("invalid hard limit accepted", value)
		}
	}
	for _, value := range []string{"--admission-pressure-cpu-some-avg10=NaN", "--admission-pressure-mode=retired", "--admission-pressure-max-wait-seconds=0", "--admission-pressure-max-defer=-1m"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", value}, io.Discard); err != nil {
			t.Fatal("ignored legacy setting rejected", value, err)
		}
	}
	disabled, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-pressure-enabled=false"}, io.Discard)
	if err != nil || disabled.admissionPressure.Enabled {
		t.Fatal("cannot disable PSI valve", err)
	}
}

func TestChartPressureRendersOnlyHardValve(t *testing.T) {
	for _, set := range []string{"admission.pressure.mode=retired", "admission.pressure.hardCpu=29", "admission.pressure.cpuSomeAvg10=101", "admission.declaredResources.cpuMillis=0"} {
		output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+goProxyDigest, "--set", set).CombinedOutput()
		if err != nil {
			t.Fatalf("retired knob affected rendering: %v %s", err, output)
		}
		text := string(output)
		for _, flag := range []string{"--admission-pressure-enabled=true", "--admission-pressure-hard-cpu=", "--admission-pressure-hard-io="} {
			if !strings.Contains(text, flag) {
				t.Fatal("missing hard valve", flag)
			}
		}
		for _, flag := range []string{"--admission-pressure-mode", "--admission-pressure-cpu-some-avg10", "--admission-pressure-max-wait", "--admission-cpu-millis", "--admission-size-weights"} {
			if strings.Contains(text, flag) {
				t.Fatal("retired knob rendered", flag)
			}
		}
	}
	for _, value := range []string{"admission.pressure.hardCpu=10", "admission.pressure.hardIo=101"} {
		if output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+goProxyDigest, "--set", value).CombinedOutput(); err == nil {
			t.Fatalf("invalid hard valve rendered: %s %s", value, output)
		}
	}
}
