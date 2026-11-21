package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestSourceCommandPreservesStartupRefusal(t *testing.T) {
	const marker = "OBERTH_TEST_SOURCE_ENTRYPOINT"
	if os.Getenv(marker) == "1" {
		os.Args = []string{"oberth-source-plugin", "one", "two"}
		main()
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSourceCommandPreservesStartupRefusal$")
	command.Env = append(os.Environ(), marker+"=1", "GORACE=atexit_sleep_ms=0")
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatal("command lost its startup refusal exit code", err)
	}
	if string(output) != "absolute Unix socket path required. Pass one absolute Unix socket path.\n" {
		t.Fatalf("startup refusal output changed: %q", output)
	}
}
