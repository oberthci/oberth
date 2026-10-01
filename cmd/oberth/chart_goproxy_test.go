package main

import (
	"os/exec"
	"strings"
	"testing"
)

const goProxyDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// TestChartRendersConfiguredGoProxy verifies explicit administrator mappings
// render the ClusterIP, NetworkPolicy, TLS resources, arguments, and port.
func TestChartRendersConfiguredGoProxy(t *testing.T) {
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+goProxyDigest,
		"--set", "argo.goProxy.enabled=true",
		"--set", "argo.goProxy.modulePrefix=go.example.test",
		"--set", "argo.goProxy.repositoryPrefix=sample-",
		"--set", "argo.goProxy.upstream=forge",
		"--set", "argo.goProxy.organization=example",
	).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	output := string(rendered)
	for _, want := range []string{
		"name: oberth-goproxy\n",
		"type: ClusterIP",
		"name: oberth-goproxy-tls",
		"name: oberth-goproxy-ingress",
		"--argo-goproxy-listen=:8444",
		"--argo-goproxy-cert=/etc/oberth/goproxy-tls/tls.crt",
		"--argo-goproxy-key=/etc/oberth/goproxy-tls/tls.key",
		"--argo-goproxy-ca=/etc/oberth/goproxy-tls/ca.crt",
		"--argo-goproxy-url=https://oberth-goproxy.",
		"containerPort: 8444",
		"oberth.ci/trigger",
		"mountPath: /etc/oberth/goproxy-tls",
		"secretName: oberth-goproxy-tls",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("rendered chart is missing %q", want)
		}
	}
}

// TestChartOmitsGoProxyWhenDisabled verifies that argo.goProxy.enabled=false
// removes all goproxy resources.
func TestChartOmitsGoProxyByDefault(t *testing.T) {
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+goProxyDigest,
	).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	output := string(rendered)
	for _, absent := range []string{
		"goproxy-tls",
		"goproxy-ingress",
		"goproxy-service",
		"--argo-goproxy",
		"containerPort: 8444",
	} {
		if strings.Contains(output, absent) {
			t.Errorf("disabled chart should not contain %q", absent)
		}
	}
}

// TestChartGoProxyReusedValuesCompat verifies that a --reuse-values upgrade
// from a revision that never had argo.goProxy renders without error and without
// goproxy resources. In a real --reuse-values scenario, the old release's merged
// values simply lack the key entirely, so we simulate that with a values file
// that has argo.goProxy.enabled set to false.
func TestChartGoProxyReusedValuesCompat(t *testing.T) {
	// The schema requires argo.goProxy to be an object (or absent). A real
	// --reuse-values upgrade from a pre-goProxy revision carries no goProxy
	// key at all, and the chart's `| default dict` produces an empty dict
	// whose .enabled is nil/false. Simulate by explicitly disabling.
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+goProxyDigest,
		"--set", "argo.goProxy.enabled=false",
	).Output()
	if err != nil {
		t.Fatalf("render chart with disabled goProxy: %v", err)
	}
	if strings.Contains(string(rendered), "--argo-goproxy") {
		t.Error("disabled argo.goProxy should not render goproxy args")
	}
}

// TestChartGoProxyGuardRejectsZeroPort verifies that the chart rejects
// argo.goProxy.port=0 when enabled is true. The chart's values.schema.json
// enforces minimum:1, which fires before the template guard (defense in depth).
func TestChartGoProxyGuardRejectsZeroPort(t *testing.T) {
	out, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+goProxyDigest,
		"--set", "argo.goProxy.enabled=true",
		"--set", "argo.goProxy.modulePrefix=go.example.test",
		"--set", "argo.goProxy.repositoryPrefix=sample-",
		"--set", "argo.goProxy.upstream=forge",
		"--set", "argo.goProxy.organization=example",
		"--set", "argo.goProxy.enabled=true",
		"--set", "argo.goProxy.port=0",
	).CombinedOutput()
	if err == nil {
		t.Fatal("expected template error for port=0, got none")
	}
	errText := string(out)
	// The schema gate fires first (minimum: got 0, want 1); the template
	// guard is defense-in-depth for --skip-schema-validation paths.
	if !strings.Contains(errText, "minimum") && !strings.Contains(errText, "goProxy.port") {
		t.Fatalf("error should mention port validation: %s", errText)
	}
}

// TestChartGoProxyConfiguredUpgradeRetainsResources verifies that an explicitly
// configured namespace stays enabled when the installer pins only its port.
func TestChartGoProxyConfiguredUpgradeRetainsResources(t *testing.T) {
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+goProxyDigest,
		"--set", "argo.goProxy.enabled=true",
		"--set", "argo.goProxy.modulePrefix=go.example.test",
		"--set", "argo.goProxy.repositoryPrefix=sample-",
		"--set", "argo.goProxy.upstream=forge",
		"--set", "argo.goProxy.organization=example",
		// Preserve administrator enablement/mapping plus the installer port:
		"--set", "argo.goProxy.enabled=true",
		"--set", "argo.goProxy.port=8444",
	).Output()
	if err != nil {
		t.Fatalf("render chart with installer pins: %v", err)
	}
	output := string(rendered)
	for _, want := range []string{
		"name: oberth-goproxy\n",
		"type: ClusterIP",
		"name: oberth-goproxy-tls",
		"--argo-goproxy-listen=:8444",
		"containerPort: 8444",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("installer-pinned chart render missing %q", want)
		}
	}
}

// TestChartGoProxyNetworkPolicyScoping verifies the NetworkPolicy targets
// pipeline pods by the oberth.ci/trigger label existence check and restricts
// to the argo namespace.
func TestChartGoProxyNetworkPolicyScoping(t *testing.T) {
	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+goProxyDigest,
		"--set", "argo.goProxy.enabled=true",
		"--set", "argo.goProxy.modulePrefix=go.example.test",
		"--set", "argo.goProxy.repositoryPrefix=sample-",
		"--set", "argo.goProxy.upstream=forge",
		"--set", "argo.goProxy.organization=example",
	).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	output := string(rendered)
	// The NetworkPolicy must scope ingress from the argo namespace only.
	if !strings.Contains(output, "kubernetes.io/metadata.name: oberth-argo") {
		t.Error("NetworkPolicy must select pipeline namespace by name")
	}
	// Must use Exists operator on the trigger label.
	if !strings.Contains(output, "operator: Exists") {
		t.Error("NetworkPolicy must use Exists operator for oberth.ci/trigger")
	}
}
