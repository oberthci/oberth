package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestSecondInterruptForcesExit(t *testing.T) {
	if os.Getenv("C11_SIGNAL_CHILD") == "1" {
		signalChild(t)
		return
	}
	child := startSignalChild(t)
	child.line(t, "ready")
	child.interrupt(t)
	child.line(t, "stopping")
	child.interrupt(t)
	child.waitForced(t)
}
func signalChild(t *testing.T) {
	ctx, stop := interruptContext(t.Context())
	defer stop()
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	<-ctx.Done()
	_, _ = fmt.Fprintln(os.Stdout, "stopping")
	select {}
}

type signalProcess struct {
	command *exec.Cmd
	output  *bufio.Scanner
	done    chan error
	joined  bool
}

func startSignalChild(t *testing.T) *signalProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSecondInterruptForcesExit$", "-test.timeout=15s")
	cmd.Env = append(os.Environ(), "C11_SIGNAL_CHILD=1")
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &signalProcess{command: cmd, output: bufio.NewScanner(output), done: make(chan error, 1)}
	go func() { child.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !child.joined {
			_ = cmd.Process.Kill()
			<-child.done
		}
	})
	return child
}
func (c *signalProcess) line(t *testing.T, want string) {
	t.Helper()
	if !c.output.Scan() || c.output.Text() != want {
		t.Fatal("signal helper did not reach " + want)
	}
}
func (c *signalProcess) interrupt(t *testing.T) {
	t.Helper()
	if err := c.command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
}
func (c *signalProcess) waitForced(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	var err error
	select {
	case err = <-c.done:
		c.joined = true
	case <-timer.C:
		t.Fatal("second interrupt was swallowed")
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("second interrupt did not force exit: %v", err)
	}
	if exit.ExitCode() != 130 && c.command.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGINT {
		t.Fatalf("wrong forced exit: %v", err)
	}
}
