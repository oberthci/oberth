package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSigningClosesCallerInputAndOverridesAmbientPassword(t *testing.T) {
	env := signingEnvironment(t)
	env["COSIGN_PASSWORD"] = "ambient-password"
	command := exec.CommandContext(t.Context(), "bash", "../../hack/release.sh", "sign")
	command.Stdin = strings.NewReader("caller input must not reach signing\n")
	command.Env = scriptEnv()
	for key, value := range env {
		command.Env = append(command.Env, key+"="+value)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal("signing required input", err, string(output))
	}
}

func TestSignerStandInRefusesUnsetPasswordWithDeviceInput(t *testing.T) {
	env := signingEnvironment(t)
	command := exec.CommandContext(t.Context(), env["SIGNER"], "sign-blob", "--key", env["EXPECTED_KEY"], "--bundle", filepath.Join(env["RELEASE_DIR"], "bundle"))
	for _, value := range scriptEnv() {
		if !strings.HasPrefix(value, "COSIGN_PASSWORD=") {
			command.Env = append(command.Env, value)
		}
	}
	for key, value := range env {
		command.Env = append(command.Env, key+"="+value)
	}
	if err := command.Run(); err == nil {
		t.Fatal("stand-in accepted an unset password with closed device input")
	}
}
