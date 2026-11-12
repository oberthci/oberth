package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The removed watchTunnel value is refused by the chart schema, never
// ignored into a render.
func TestChartRefusesRemovedInstanceConnectorValues(t *testing.T) {
	out, err := exec.Command("helm", "template", "product", "../../charts/oberth", "--namespace", "ci",
		"--set-string", "image.ref=example.invalid/oberth@"+goProxyDigest,
		"--set", "watchTunnel.enabled=true").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "watchTunnel") {
		t.Fatalf("removed watchTunnel rendered: %v\n%s", err, out)
	}
}
