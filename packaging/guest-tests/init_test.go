//go:build linux

package main

import (
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestGuestRunnerIsolation(t *testing.T) {
	command := exec.Command("python3", "-I", "-B", "test_run.py")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("guest runner contract: %v\n%s", err, output)
	}
}

func TestOutputCountsNestedSkips(t *testing.T) {
	passed, skipped, err := testOutput(strings.NewReader("=== RUN TestOne\n--- PASS: TestOne (1s)\n    --- SKIP: TestTwo/subtest (0s)\nPASS\n"), io.Discard)
	if err != nil || passed != 1 || skipped != 1 {
		t.Fatalf("passes=%d skips=%d error=%v", passed, skipped, err)
	}
}

func TestOutputDoesNotAcceptBarePass(t *testing.T) {
	passed, skipped, err := testOutput(strings.NewReader("PASS\n"), io.Discard)
	if err != nil || passed != 0 || skipped != 0 {
		t.Fatalf("passes=%d skips=%d error=%v", passed, skipped, err)
	}
}
