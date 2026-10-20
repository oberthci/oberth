package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/service"
)

func TestServeParsesAndChartRendersMaxHeadWait(t *testing.T) {
	options, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines"}, io.Discard)
	if err != nil || options.admissionMaxHeadWait != service.DefaultMaxHeadWait {
		t.Fatalf("default max head wait = %s, %v", options.admissionMaxHeadWait, err)
	}
	options, err = parseServeOptions([]string{"--argo-namespace", "argo-pipelines", "--admission-max-head-wait", "0"}, io.Discard)
	if err != nil || options.admissionMaxHeadWait != 0 {
		t.Fatalf("strict FIFO = %s, %v", options.admissionMaxHeadWait, err)
	}
	if _, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines", "--admission-max-head-wait", "-1m"}, io.Discard); err == nil {
		t.Fatal("a negative bound was accepted")
	}
	for value, want := range map[string]string{"": "--admission-max-head-wait=20m", "45m": "--admission-max-head-wait=45m"} {
		arguments := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + goProxyDigest}
		if value != "" {
			arguments = append(arguments, "--set-string", "admission.maxHeadWait="+value)
		}
		output, err := exec.Command("helm", arguments...).CombinedOutput()
		if err != nil || !strings.Contains(string(output), want) {
			t.Fatalf("render %q: %v, missing %q", value, err, want)
		}
	}
	if output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+goProxyDigest,
		"--set-string", "admission.maxHeadWait=soon").CombinedOutput(); err == nil {
		t.Fatalf("an invalid bound rendered:\n%s", output)
	}
}
