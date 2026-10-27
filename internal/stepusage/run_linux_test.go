//go:build linux

package stepusage

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestRunChildSignalAndTerminationMessage(t *testing.T) {
	for _, tc := range []struct {
		name, script, message string
		code                  int
	}{
		{"segv", "ulimit -c 0; kill -SEGV $$", "", 139},
		{"message", `printf 'step diagnostic' > "$1"; exit 23`, "step diagnostic", 23},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			summary := filepath.Join(root, "termination")
			if err := os.WriteFile(summary, nil, 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"--", "/bin/sh", "-c", tc.script, "step", summary}, nil, &stdout, &stderr, "testdata/v2", "missing", summary); code != tc.code {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			raw, err := os.ReadFile(summary)
			if err != nil {
				t.Fatal(err)
			}
			if tc.message != "" {
				if string(raw) != tc.message {
					t.Fatalf("message overwritten: %q", raw)
				}
				if u := Decode(string(raw)); u != (Usage{}) {
					t.Fatalf("step message became usage: %+v", u)
				}
			} else if u := Decode(string(raw)); u.MaxRSSBytes == nil || *u.MaxRSSBytes <= 0 {
				t.Fatalf("signaled child lost usage: %q", raw)
			}
		})
	}
}

func TestUsageRunnerHelper(t *testing.T) {
	if os.Getenv("OBERTH_USAGE_TEST_HELPER") != "1" {
		return
	}
	os.Exit(run([]string{"--", "/bin/sh", "-c", os.Getenv("OBERTH_USAGE_TEST_SCRIPT")}, os.Stdin, os.Stdout, os.Stderr, "testdata/v2", os.Getenv("OBERTH_USAGE_SELF"), os.Getenv("OBERTH_USAGE_TERMINATION")))
}

func TestRunForwardsSignalsAndKilledWrapperLeavesUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
	}{
		{"TERM", syscall.SIGTERM}, {"INT", syscall.SIGINT}, {"HUP", syscall.SIGHUP},
		{"QUIT", syscall.SIGQUIT}, {"USR1", syscall.SIGUSR1}, {"USR2", syscall.SIGUSR2},
		{"WINCH", syscall.SIGWINCH}, {"KILL", syscall.SIGKILL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			root := t.TempDir()
			self, summary := filepath.Join(root, "self"), filepath.Join(root, "termination")
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
			script := `trap 'exit 42' ` + tc.name + `; printf '%s\n' "$$"; read line`
			if tc.signal == syscall.SIGKILL {
				script = `printf '%s\n' "$$"; read line`
			}
			cmd.Env = append(os.Environ(), "OBERTH_USAGE_TEST_HELPER=1", "OBERTH_USAGE_SELF="+self, "OBERTH_USAGE_TERMINATION="+summary, "OBERTH_USAGE_TEST_SCRIPT="+script)
			cmd.Stdin = input
			// A SIGKILLed wrapper leaves a child briefly alive. Use a file,
			// not an exec copier pipe whose EOF would depend on that child.
			stderr, err := os.CreateTemp(root, "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stderr.Close() }()
			cmd.Stderr = stderr
			readStderr := func() string { raw, _ := os.ReadFile(stderr.Name()); return string(raw) }
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Readiness comes after installing the child's trap; no sleep races.
			var pid int
			if _, err := fmt.Fscanln(bufio.NewReader(output), &pid); err != nil || pid <= 0 {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("no ready child: %v %s", err, readStderr())
			}
			defer func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }()
			if err := cmd.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("exit=%v stderr=%s", err, readStderr())
			}
			raw, err := os.ReadFile(summary)
			if err != nil {
				t.Fatal(err)
			}
			if tc.signal == syscall.SIGKILL {
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL || len(raw) != 0 || Decode(string(raw)) != (Usage{}) {
					t.Fatalf("killed wrapper: status=%v summary=%q", exit, raw)
				}
			} else if exit.ExitCode() != 42 || Decode(string(raw)).MaxRSSBytes == nil {
				t.Fatalf("signal not forwarded: exit=%v summary=%q stderr=%s", exit, raw, readStderr())
			}
		})
	}
}
