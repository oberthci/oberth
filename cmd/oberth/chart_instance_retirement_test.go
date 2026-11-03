package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestChartIgnoresRetiredInstanceConnectorValues(t *testing.T) {
	// Reused release values may contain obsolete settings that were valid for
	// the previous chart. They must not create connectors or reject an upgrade.
	out, err := exec.Command("helm", "template", "product", "../../charts/oberth", "--namespace", "ci",
		"--set-string", "image.ref=example.invalid/oberth@"+goProxyDigest,
		"--set", "watchTunnel.enabled=true", "--set-string", "watchTunnel.originCACert=obsolete-value",
		"--set", "platformProtection.enabled=true").CombinedOutput()
	if err != nil {
		t.Fatalf("obsolete instance values rejected: %v\n%s", err, out)
	}
	for _, forbidden := range []string{"cloudflared", "tunnel-connector"} {
		if strings.Contains(string(out), forbidden) {
			t.Errorf("retired instance object rendered: %s", forbidden)
		}
	}
}
