package api

import (
	"os/exec"
	"testing"
)

func TestDashboardPromptsOnAuthenticationRateLimit(t *testing.T) {
	command := exec.Command("node", "../../hack/test-api-auth-prompt.mjs")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dashboard authentication prompt: %v\n%s", err, output)
	}
}
