package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestServeAndChartIgnoreResourceDrainControls(t *testing.T) {
	if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-max-head-wait=-1m"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"20m", "45m", "soon"} {
		output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+goProxyDigest, "--set-string", "admission.maxHeadWait="+value).CombinedOutput()
		if err != nil || strings.Contains(string(output), "--admission-max-head-wait") {
			t.Fatalf("retired drain control rendered: %v %s", err, output)
		}
	}
}
