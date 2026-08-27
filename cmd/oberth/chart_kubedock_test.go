package main

import (
	"os/exec"
	"strings"
	"testing"
)

const kubedockTestDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// Kubedock lived in a stray manifest applied by hand into a namespace the
// chart owns, next to a values file that had to be applied separately for the
// shim to be reachable. Two manual steps that only work together is two
// chances to end up with half a setup.
func TestChartDeploysKubedockIntoThePipelineNamespace(t *testing.T) {
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+kubedockTestDigest,
		"--set", "argo.namespace=oberth-argo",
		"--set", "kubedock.enabled=true",
	).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	output := string(rendered)
	for _, want := range []string{
		"joyrex2001/kubedock",
		"--reverse-proxy",
		"--namespace=oberth-argo",
		// The Service name is what a pipeline's DOCKER_HOST points at, so it
		// is fixed rather than derived from the release name.
		"\n  name: kubedock\n",
		// Headless: the reverse proxy opens a fresh port per test container,
		// and a ClusterIP Service can only forward ports named up front.
		"clusterIP: None",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("rendered chart is missing %q", want)
		}
	}
	// The blast radius is the Role. A shim that could reach the server's own
	// namespace would be strictly worse than the Docker socket it replaces.
	if strings.Contains(output, "ClusterRole\nmetadata:\n  name: oberth-kubedock") {
		t.Error("kubedock was granted cluster-wide permissions")
	}
}

// Off unless asked for: it is a capability a repository needs, not a
// dependency of Oberth.
func TestChartOmitsKubedockByDefault(t *testing.T) {
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+kubedockTestDigest,
	).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	if strings.Contains(string(rendered), "kubedock") {
		t.Error("kubedock is deployed without being asked for")
	}
}
