package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestServePressureDefaultsAndValidation(t *testing.T) {
	defaults, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines"}, io.Discard)
	if err != nil || !defaults.admissionPressure.Enabled || defaults.admissionPressure.CPUSomeAvg10 != 40 || defaults.admissionPressure.IOSomeAvg10 != 25 || defaults.admissionPressure.HardCPU != 70 || defaults.admissionPressure.HardIO != 50 || defaults.admissionPressure.MaxWaitSeconds != 1200 || defaults.admissionPressure.Mode != "enforce" || defaults.admissionPressure.IOFullAvg60Max != 0 || defaults.admissionPressure.CPUSomeAvg60Max != 0 || defaults.admissionPressure.Hysteresis != 10 || defaults.admissionPressure.MaxDefer != 20*time.Minute {
		t.Fatal("bad pressure defaults", defaults.admissionPressure, err)
	}
	for _, args := range [][]string{
		{"--admission-pressure-cpu-some-avg10=NaN"},
		{"--admission-pressure-io-some-avg10=10"},
		{"--admission-pressure-hard-cpu=39"},
		{"--admission-pressure-hard-io=Inf"},
		{"--admission-pressure-max-wait-seconds=0"},
		{"--admission-pressure-mode=typo"},
		{"--admission-pressure-mode="},
		{"--admission-pressure-io-full-avg60-max=NaN"},
		{"--admission-pressure-cpu-some-avg60-max=Inf"},
		{"--admission-pressure-io-full-avg60-max=101"},
		{"--admission-pressure-io-full-avg60-max=-1"},
		{"--admission-pressure-io-full-avg60-max=30", "--admission-pressure-hysteresis=30"},
		{"--admission-pressure-hysteresis=-1"},
		{"--admission-pressure-io-full-avg60-max=30", "--admission-pressure-max-defer=0"},
		{"--admission-pressure-max-defer=-1m"},
	} {
		if _, err := parseServeOptions(append([]string{"--argo-namespace", "argo-pipelines"}, args...), io.Discard); err == nil {
			t.Fatal("invalid pressure flags accepted", args)
		}
	}
	enforced, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines", "--admission-pressure-mode=enforce"}, io.Discard)
	if err != nil || enforced.admissionPressure.Mode != "enforce" {
		t.Fatal("explicit enforcement rejected", err)
	}
	disabled, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines", "--admission-pressure-enabled=false", "--admission-pressure-max-defer=0"}, io.Discard)
	if err != nil || disabled.admissionPressure.Enabled || disabled.admissionPressure.IOFullAvg60Max != 0 {
		t.Fatal("explicitly disabling pressure rejected", err)
	}
}

func TestChartPressureDefaultsOverridesAndExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		set  []string
		want []string
	}{
		{want: []string{"--admission-pressure-enabled=true", "--admission-pressure-cpu-some-avg10=40", "--admission-pressure-io-some-avg10=25", "--admission-pressure-hard-cpu=70", "--admission-pressure-hard-io=50", "--admission-pressure-max-wait-seconds=1200", "--admission-pressure-mode=enforce", "--admission-pressure-io-full-avg60-max=0", "--admission-pressure-cpu-some-avg60-max=0", "--admission-pressure-hysteresis=10", "--admission-pressure-max-defer=20m"}},
		{set: []string{"admission.pressure.enabled=false", "admission.pressure.ioFullAvg60Max=0", "admission.pressure.hysteresis=0", "admission.pressure.maxDefer=0s"}, want: []string{"--admission-pressure-enabled=false", "--admission-pressure-io-full-avg60-max=0", "--admission-pressure-hysteresis=0"}},
		{set: []string{"admission.pressure.cpuSomeAvg10=45.5", "admission.pressure.ioSomeAvg10=30", "admission.pressure.hardCpu=75", "admission.pressure.hardIo=55", "admission.pressure.maxWaitSeconds=60"}, want: []string{"--admission-pressure-cpu-some-avg10=45.5", "--admission-pressure-io-some-avg10=30", "--admission-pressure-hard-cpu=75", "--admission-pressure-hard-io=55", "--admission-pressure-max-wait-seconds=60"}},
		{set: []string{"admission.pressure.mode=enforce", "admission.pressure.ioFullAvg60Max=45.5", "admission.pressure.cpuSomeAvg60Max=70", "admission.pressure.hysteresis=5", "admission.pressure.maxDefer=7m"}, want: []string{"--admission-pressure-mode=enforce", "--admission-pressure-io-full-avg60-max=45.5", "--admission-pressure-cpu-some-avg60-max=70", "--admission-pressure-hysteresis=5", "--admission-pressure-max-defer=7m"}},
	} {
		args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + goProxyDigest}
		for _, value := range tc.set {
			mode := "--set"
			if strings.Contains(value, "=45.5") {
				mode = "--set-json"
			}
			args = append(args, mode, value)
		}
		output, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("render: %v %s", err, output)
		}
		for _, want := range tc.want {
			if !strings.Contains(string(output), want) {
				t.Fatal("missing pressure flag", want)
			}
		}
	}
	for _, value := range []string{"admission.pressure.mode=typo", "admission.pressure.ioFullAvg60Max=-1", "admission.pressure.cpuSomeAvg60Max=101", "admission.pressure.maxDefer=soon", "admission.pressure.ioFullAvg60Max=5", "admission.pressure.cpuSomeAvg60Max=10", "admission.pressure.cpuSomeAvg10=10", "admission.pressure.ioSomeAvg10=101", "admission.pressure.hardCpu=39", "admission.pressure.hardIo=24", "admission.pressure.maxWaitSeconds=0"} {
		if output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+goProxyDigest, "--set", value).CombinedOutput(); err == nil {
			t.Fatalf("invalid pressure chart value accepted: %s %s", value, output)
		}
	}
	for _, duration := range []string{"0", "0s", "0m0s", "0.0s"} {
		if output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+goProxyDigest, "--set", "admission.pressure.ioFullAvg60Max=30", "--set", "admission.pressure.maxDefer="+duration).CombinedOutput(); err == nil {
			t.Fatalf("invalid legacy maxDefer accepted: %s %s", duration, output)
		}
	}

}
