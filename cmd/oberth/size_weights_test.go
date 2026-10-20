package main

import (
	"io"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/service"
)

func TestServeParsesAdmissionSizeWeights(t *testing.T) {
	options, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines"}, io.Discard)
	if err != nil || options.sizeWeights != service.DefaultSizeWeights {
		t.Fatalf("default weights = %+v, %v", options.sizeWeights, err)
	}
	options, err = parseServeOptions([]string{"--argo-namespace", "argo-pipelines", "--admission-size-weights", "S=1,M=2,L=3,XL=11"}, io.Discard)
	if err != nil || options.sizeWeights != (service.SizeWeights{S: 1, M: 2, L: 3, XL: 11}) {
		t.Fatalf("XL=11 weights = %+v, %v", options.sizeWeights, err)
	}
	if _, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines", "--admission-size-weights", "S=3,M=2"}, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "S <= M <= L") {
		t.Fatalf("invalid weights = %v", err)
	}
}

// The chart renders the weights into the server flag, so a restarted (or
// upgraded) server keeps them, and refuses an invalid set at render time.
func TestChartRendersAdmissionSizeWeights(t *testing.T) {
	render := func(arguments ...string) (string, error) {
		output, err := exec.Command("helm", append([]string{"template", "oberth", "../../charts/oberth",
			"--set", "image.ref=example.invalid/oberth@" + goProxyDigest}, arguments...)...).CombinedOutput()
		return string(output), err
	}
	flag := regexp.MustCompile(`--admission-size-weights=(\S+)`)
	for _, testCase := range []struct {
		set  []string
		want service.SizeWeights
	}{
		{nil, service.DefaultSizeWeights},
		{[]string{"--set", "admission.sizeWeights.XL=11"}, service.SizeWeights{S: 1, M: 2, L: 3, XL: 11}},
	} {
		output, err := render(testCase.set...)
		if err != nil {
			t.Fatalf("render %v: %v\n%s", testCase.set, err, output)
		}
		match := flag.FindStringSubmatch(output)
		if match == nil {
			t.Fatalf("render %v has no --admission-size-weights", testCase.set)
		}
		parsed, err := service.ParseSizeWeights(strings.Trim(match[1], `"`))
		if err != nil || parsed != testCase.want {
			t.Fatalf("render %v weights = %+v, %v; want %+v", testCase.set, parsed, err, testCase.want)
		}
	}
	for _, invalid := range [][]string{
		{"--set", "admission.sizeWeights.XL=2"},
		{"--set", "admission.sizeWeights.S=0"},
	} {
		if output, err := render(invalid...); err == nil {
			t.Fatalf("render %v accepted invalid weights:\n%s", invalid, output)
		}
	}
}
