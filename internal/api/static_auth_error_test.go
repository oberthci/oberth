package api

import (
	"os/exec"
	"testing"
)

func TestDashboardHandlesStreamAuthError(t *testing.T) {
	command := exec.Command("node", "../../hack/test-live-auth-error.mjs")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("stream auth behavior: %v\n%s", err, out)
	}
}

func TestDashboardPromptsOnAuthenticationRateLimit(t *testing.T) {
	command := exec.Command("node", "../../hack/test-api-auth-prompt.mjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dashboard authentication prompt: %v\n%s", err, output)
	}
}
