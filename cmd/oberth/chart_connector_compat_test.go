package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDeferredConnectorRemainsRenderable(t *testing.T) {
	out, err := exec.Command("helm", "template", "oberth", "../../charts/oberth", "--namespace", "ci", "--set", "watchTunnel.enabled=true", "--set", "platformProtection.enabled=true").CombinedOutput()
	if err != nil {
		t.Fatalf("render retained connector: %v\n%s", err, out)
	}
	for _, required := range []string{"name: cloudflared-watch-oberth-v2", "app.kubernetes.io/component: tunnel-connector"} {
		if !strings.Contains(string(out), required) {
			t.Errorf("holdable chart lost connector object: %s", required)
		}
	}
}
