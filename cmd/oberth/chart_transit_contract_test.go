package main

import (
	"bufio"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestProductionChartTrustedTransitArgumentsParse(t *testing.T) {
	const serverImage = "example.invalid/oberth@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	command := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--show-only", "templates/deployment.yaml",
		"--set", "image.ref="+serverImage,
		"--set", "secretstore.enabled=true",
		"--set", "secretstore.address=https://openbao.oberth.svc:8200",
		"--set", "secretstore.role=oberth-ci",
		"--set", "secretstore.transit.enabled=true",
	)
	rendered, err := command.Output()
	if err != nil {
		t.Fatalf("render production chart: %v", err)
	}
	var arguments []string
	scanner := bufio.NewScanner(strings.NewReader(string(rendered)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "- --") {
			arguments = append(arguments, strings.TrimPrefix(line, "- "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	options, err := parseServeOptions(arguments, io.Discard)
	if err != nil {
		t.Fatalf("chart-emitted serve arguments do not parse: %v\n%v", err, arguments)
	}
	if options.argoNamespace != "oberth-argo" {
		t.Fatalf("chart-emitted --argo-namespace = %q, want %q", options.argoNamespace, "oberth-argo")
	}
	if options.secretStoreTransitMount != "oberth-transit" || options.secretStoreTransitKey != "trusted-plan-artifacts" ||
		options.secretStoreAddress != "https://openbao.oberth.svc:8200" || options.secretStoreInsecureHTTP {
		t.Fatalf("chart-emitted trusted transport = %+v", options)
	}
}

func TestChartRejectsTrustedTransitOverDevelopmentHTTP(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	base := []string{"template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@" + digest,
		"--set", "secretstore.enabled=true",
		"--set", "secretstore.address=http://openbao.openbao.svc:8200",
		"--set", "secretstore.insecureHTTPForDev=true",
		"--set", "secretstore.role=oberth-ci",
	}
	enabled := append(append([]string{}, base...), "--set", "secretstore.transit.enabled=true")
	if output, err := exec.Command("helm", enabled...).CombinedOutput(); err == nil || !strings.Contains(string(output), "requires verified HTTPS") {
		t.Fatalf("HTTP transit render error = %v\n%s", err, output)
	}
	disabled := append(base, "--set", "secretstore.transit.enabled=false")
	if output, err := exec.Command("helm", disabled...).CombinedOutput(); err != nil {
		t.Fatalf("explicitly transit-disabled development render failed: %v\n%s", err, output)
	}
}
