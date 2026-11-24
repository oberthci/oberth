package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseHelpListsExecutableFlags(t *testing.T) {
	t.Parallel()
	binary := releaseHelpBinary(t)
	var want bytes.Buffer
	flags := releaseFlags(new(options))
	flags.SetOutput(&want)
	flags.Usage()
	if !strings.Contains(want.String(), "--registry <host>") {
		t.Fatal("registry login usage omits the required host")
	}
	for _, operation := range []string{"", "base", "package", "key", "pin", "signature", "verify", "publish", "finalize"} {
		for _, form := range []string{"--help", "-h"} {
			t.Run(strings.TrimSpace(operation+" "+form), func(t *testing.T) {
				args := []string{form}
				if operation != "" {
					args = []string{operation, form}
				}
				assertReleaseHelpCommand(t, binary, args, want.String())
			})
		}
	}
}

func releaseHelpBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "oberth-release")
	command := exec.CommandContext(t.Context(), "go", "build", "-p=2", "-buildvcs=false", "-o", binary, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build release tool: %v: %s", err, output)
	}
	return binary
}

func assertReleaseHelpCommand(t *testing.T, binary string, args []string, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = t.TempDir()
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("help must succeed: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("help must print only registered flags on stdout: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	files, err := os.ReadDir(command.Dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("help must leave its empty working directory unchanged: %v: %v", files, err)
	}
}
