package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleasePinScriptRefusalHasOnlyNativeGuidance(t *testing.T) {
	input := pinInput(t, []byte("not a public key"))
	scratch := t.TempDir()
	command := exec.CommandContext(t.Context(), "sh", "../../hack/release-pin.sh", input)
	command.Env = append(os.Environ(), "GOTMPDIR="+scratch)
	command.WaitDelay = 5 * time.Second
	output, err := command.CombinedOutput()
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 1 {
		t.Fatal("script did not preserve refusal exit status", err)
	}
	expected := keyInvalid.Error() + "\n" + keyInvalid.Remedy() + "\n"
	if string(output) != expected {
		t.Fatal("refusal is not exactly the native reason and remedy")
	}
	if strings.Contains(string(output), "BEGIN") || strings.Contains(string(output), "exit status") {
		t.Fatal("refusal printed key material or tool runner noise")
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatal("temporary helper was retained", err)
	}
}

func TestReleasePinScriptUsageOnlyOffersProvisioning(t *testing.T) {
	command := exec.CommandContext(t.Context(), "sh", "../../hack/release-pin.sh")
	command.Env = append(os.Environ(), "GOTMPDIR="+t.TempDir())
	command.WaitDelay = 5 * time.Second
	output, err := command.CombinedOutput()
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 1 {
		t.Fatal("script did not refuse missing provisioning input", err)
	}
	if string(output) != "Use hack/release-pin.sh [--replace] <public-key-file>.\n" {
		t.Fatal("usage does not give exactly the supported provisioning command", string(output))
	}
}

func TestReleasePinScriptUnknownFlagsReturnUsageBeforeTools(t *testing.T) {
	for _, args := range [][]string{{"--check"}, {"--unknown"}, {"-x"}, {"-"}, {"--"}, {"--replace", "--check"}, {"--replace", "-x"}, {"key.pem", "--unknown"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			scratch := t.TempDir()
			command := exec.CommandContext(t.Context(), "sh", append([]string{"../../hack/release-pin.sh"}, args...)...)
			command.Env = append(os.Environ(), "PATH="+scratch, "GOTMPDIR="+filepath.Join(scratch, "cache"))
			command.WaitDelay = 5 * time.Second
			output, err := command.CombinedOutput()
			var status *exec.ExitError
			if !errors.As(err, &status) || status.ExitCode() != 1 {
				t.Fatal("unknown flag did not get an immediate usage refusal", err)
			}
			if string(output) != "Use hack/release-pin.sh [--replace] <public-key-file>.\n" {
				t.Fatal("unknown flag did not get exactly the usage line", string(output))
			}
			if entries, err := os.ReadDir(scratch); err != nil || len(entries) != 0 {
				t.Fatal("unknown flag created temporary build state", err)
			}
		})
	}
}
