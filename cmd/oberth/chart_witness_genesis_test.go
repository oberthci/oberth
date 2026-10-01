package main

import (
	"os/exec"
	"strings"
	"testing"
)

// T18: Chart render guards for acceptWitnessGenesis
func TestChartWitnessGenesisRenderGuards(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	base := []string{"template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@" + digest,
	}

	t.Run("genesis without rekor fails", func(t *testing.T) {
		args := append(append([]string{}, base...),
			"--set-string", "auditAnchor.acceptWitnessGenesis=42:"+strings.Repeat("ab", 32))
		output, err := exec.Command("helm", args...).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "requires auditAnchor.rekorURL") {
			t.Fatalf("genesis without rekor: error=%v\n%s", err, output)
		}
	})

	t.Run("genesis and reset mutually exclusive", func(t *testing.T) {
		args := append(append([]string{}, base...),
			"--set", "auditAnchor.rekorURL=https://rekor.example.test",
			"--set-string", "auditAnchor.acceptWitnessGenesis=42:"+strings.Repeat("ab", 32),
			"--set-string", "auditAnchor.acceptWitnessChainReset="+strings.Repeat("cc", 32))
		output, err := exec.Command("helm", args...).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "mutually exclusive") {
			t.Fatalf("genesis+reset: error=%v\n%s", err, output)
		}
	})

	t.Run("genesis arg emitted when set", func(t *testing.T) {
		genesisValue := "42:" + strings.Repeat("ab", 32)
		args := append(append([]string{}, base...),
			"--set", "auditAnchor.rekorURL=https://rekor.example.test",
			"--set-string", "auditAnchor.acceptWitnessGenesis="+genesisValue,
			"--show-only", "templates/deployment.yaml")
		output, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("render with genesis: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "--accept-witness-genesis="+genesisValue) {
			t.Fatalf("rendered deployment does not contain the genesis arg:\n%s", output)
		}
	})

	t.Run("genesis arg omitted when empty", func(t *testing.T) {
		args := append(append([]string{}, base...),
			"--show-only", "templates/deployment.yaml")
		output, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("render without genesis: %v\n%s", err, output)
		}
		if strings.Contains(string(output), "accept-witness-genesis") {
			t.Fatalf("rendered deployment contains genesis arg when empty:\n%s", output)
		}
	})
}
