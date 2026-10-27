//go:build linux

package stepusage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRunPreservesStreamsExitAndWritesRealRSS(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(root, "self")
	summary := filepath.Join(root, "termination")
	for name, raw := range map[string]string{self: "0::/\n", summary: ""} {
		if err := os.WriteFile(name, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--", "/bin/sh", "-c", "cat; printf err >&2; exit 23"}, bytes.NewBufferString("input"), &stdout, &stderr, "testdata/v2", self, summary)
	if code != 23 || stdout.String() != "input" || stderr.String() != "err" {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	raw, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	u := Decode(string(raw))
	if u.MaxRSSBytes == nil || *u.MaxRSSBytes <= 0 || u.MemoryPeakBytes == nil || *u.MemoryPeakBytes != 120<<20 || u.UserCPU == nil || *u.UserCPU != 1500*time.Millisecond {
		t.Fatalf("summary %s", raw)
	}
	// Telemetry loss, including an unwritable/missing termination file, cannot
	// turn a successful command into failure or manufacture CPU counters.
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--", "/bin/sh", "-c", "exit 0"}, nil, &stdout, &stderr, "missing", self, filepath.Join(root, "missing", "termination")); code != 0 {
		t.Fatalf("telemetry failed command: %d", code)
	}
	if code := run([]string{"--", "/no/such/command"}, nil, &stdout, &stderr, "missing", self, summary); code != 127 {
		t.Fatalf("start code %d", code)
	}
}

func TestUsageRunnerHelper(t *testing.T) {
	if os.Getenv("OBERTH_USAGE_TEST_HELPER") != "1" {
		return
	}
	os.Exit(run([]string{"--", "/bin/sh", "-c", "trap 'exit 42' TERM; printf ready; read line"}, os.Stdin, os.Stdout, os.Stderr, "testdata/v2", os.Getenv("OBERTH_USAGE_SELF"), os.Getenv("OBERTH_USAGE_TERMINATION")))
}

func TestRunForwardsTerminationAndPreservesTrapExit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	self := filepath.Join(root, "self")
	summary := filepath.Join(root, "termination")
	for name, raw := range map[string]string{self: "0::/\n", summary: ""} {
		if err := os.WriteFile(name, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	input, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = hold.Close() }()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestUsageRunnerHelper$")
	cmd.Env = append(os.Environ(), "OBERTH_USAGE_TEST_HELPER=1", "OBERTH_USAGE_SELF="+self, "OBERTH_USAGE_TERMINATION="+summary)
	cmd.Stdin = input
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, 5)
	if _, err := io.ReadFull(output, ready); err != nil {
		t.Fatalf("no ready: %v %s", err, &stderr)
	}
	if string(ready) != "ready" {
		t.Fatalf("unexpected output %q", ready)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 42 {
		t.Fatalf("exit=%v stderr=%s", err, &stderr)
	}
	raw, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	if got := Decode(string(raw)); got.MaxRSSBytes == nil || *got.MaxRSSBytes <= 0 {
		t.Fatalf("no usage after signal: %s", raw)
	}
}
