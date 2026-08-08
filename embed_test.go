package oberth

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestSetupSecretStoreScriptIsEmbedded(t *testing.T) {
	t.Parallel()
	if !bytes.HasPrefix(SetupSecretStoreScript, []byte("#!/usr/bin/env bash")) {
		t.Fatalf("embedded setup script must start with its interpreter line, got %q", SetupSecretStoreScript[:min(40, len(SetupSecretStoreScript))])
	}
	// The embedded bytes ARE scripts/setup-secretstore.sh (go:embed), so these
	// pin the customer-facing contract of the one command itself.
	for _, want := range []string{
		"setup-secretstore.sh",
		"reads, writes, or prints a token",
		"secretstore.enabled=true",
		"oberth secretstore verify",
		"token_no_default_policy=true",
		"auth/token/revoke-self",
	} {
		if !bytes.Contains(SetupSecretStoreScript, []byte(want)) {
			t.Fatalf("embedded setup script misses %q", want)
		}
	}
}

// TestSetupSecretStoreScriptContract runs the mock-CLI contract suite: it
// proves idempotent re-runs mutate nothing, drift is refused without --force,
// cross-cluster configs fail closed, dry-run performs no writes, and the
// in-cluster branch works without kubectl. The runner image ships bash, so
// this runs in every CI test burn.
func TestSetupSecretStoreScriptContract(t *testing.T) {
	t.Parallel()
	command := exec.Command("bash", "hack/test-setup-secretstore.sh")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("setup-secretstore contract suite failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("setup-secretstore contract: OK")) {
		t.Fatalf("contract suite did not report success:\n%s", output)
	}
}
