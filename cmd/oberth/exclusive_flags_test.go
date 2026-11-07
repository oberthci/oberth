package main

import (
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// #1499: the exclusive-class allowlist is off by default and bounded.
func TestServeExclusiveClassesDefaultOffAndValidated(t *testing.T) {
	defaults, err := parseServeOptions([]string{"--argo-namespace=argo"}, io.Discard)
	if err != nil || len(defaults.exclusiveClasses) != 0 {
		t.Fatal("exclusive classes enabled by default", defaults.exclusiveClasses, err)
	}
	configured, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-exclusive-classes=kvm,gpu-lab=2"}, io.Discard)
	if err != nil || !reflect.DeepEqual(configured.exclusiveClasses, map[string]int{"kvm": 1, "gpu-lab": 2}) {
		t.Fatal("allowlist not parsed", configured.exclusiveClasses, err)
	}
	for _, value := range []string{"kvm=0", "Kvm", "exclusive/kvm", "kvm,kvm"} {
		if _, err := parseServeOptions([]string{"--argo-namespace=argo", "--admission-exclusive-classes=" + value}, io.Discard); err == nil {
			t.Fatal("invalid allowlist accepted", value)
		}
	}
}

func TestChartRendersExclusiveClassesSorted(t *testing.T) {
	render := func(sets ...string) (string, error) {
		args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@" + goProxyDigest}
		for _, set := range sets {
			args = append(args, "--set", set)
		}
		output, err := exec.Command("helm", args...).CombinedOutput()
		return string(output), err
	}
	text, err := render()
	if err != nil || strings.Contains(text, "--admission-exclusive-classes") {
		t.Fatalf("default chart enables exclusive classes: %v %s", err, text)
	}
	text, err = render("admission.exclusiveClasses.kvm=1", "admission.exclusiveClasses.gpu-lab=2")
	if err != nil || !strings.Contains(text, `"--admission-exclusive-classes=gpu-lab=2,kvm=1"`) {
		t.Fatalf("allowlist not rendered: %v %s", err, text)
	}
	for _, set := range []string{"admission.exclusiveClasses.Bad=1", "admission.exclusiveClasses.kvm=0", "admission.exclusiveClasses.kvm=65"} {
		if _, err := render(set); err == nil {
			t.Fatal("chart schema accepted", set)
		}
	}
}
