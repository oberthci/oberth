package api

import (
	"os/exec"
	"testing"
)

func TestNetworkPolicyStatusDiagnostic(t *testing.T) {
	output, err := exec.Command("node", "../../hack/test-network-policy-status.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("NetworkPolicy diagnostic rendering: %v\n%s", err, output)
	}
}
