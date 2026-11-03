package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/service"
)

func TestServeIgnoresAdmissionSizeWeights(t *testing.T) {
	for _, value := range []string{"S=1,M=2,L=3,XL=11", "S=3,M=2", "retired"} {
		options, err := parseServeOptions([]string{"--argo-namespace=argo-pipelines", "--admission-size-weights=" + value}, io.Discard)
		if err != nil || options.sizeWeights != (service.SizeWeights{}) {
			t.Fatalf("deprecated weights affect admission: %+v %v", options.sizeWeights, err)
		}
	}
}

func TestChartOmitsDeprecatedAdmissionSizeWeights(t *testing.T) {
	for _, value := range []string{"admission.sizeWeights.XL=11", "admission.sizeWeights.S=0"} {
		output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@"+goProxyDigest, "--set", value).CombinedOutput()
		if err != nil || strings.Contains(string(output), "--admission-size-weights") {
			t.Fatalf("deprecated weights rendered: %v %s", err, output)
		}
	}
}
