package gitcache

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestOwnedCommandCancelsGroupAndKeepsCause(t *testing.T) {
	if mode := os.Getenv("OBERTH_C6_PROCESS_TEST"); mode != "" {
		processHelper(mode)
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	unrelated, reader := helperCommand(t, binary, "unrelated")
	if err = unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
	var parent, child int
	if _, err = fmt.Fscanf(reader, "%d %d\n", &parent, &child); err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	command, ready := helperCommand(t, binary, "leader")
	cause := errors.New("test cancellation")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(cause)
	done := make(chan error, 1)
	go func() { done <- runOwned(ctx, command) }()
	if _, err = fmt.Fscanf(ready, "%d %d\n", &parent, &child); err != nil {
		t.Fatal(err)
	}
	_ = ready.Close()
	cleanup := pinHelpers(t, parent, child)
	defer cleanup()
	cancel(cause)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err = <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("lost cancellation cause: %v", err)
		}
	case <-timer.C:
		t.Fatal("owned process cleanup did not finish")
	}
	if err = syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("descendant was not reaped: %v", err)
	}
	if err = unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated process was stopped: %v", err)
	}
}
func helperCommand(t *testing.T, binary, mode string) (*exec.Cmd, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	cmd := exec.Command(binary, "-test.run=^TestOwnedCommandCancelsGroupAndKeepsCause$")
	cmd.Env = append(os.Environ(), "OBERTH_C6_PROCESS_TEST="+mode)
	cmd.ExtraFiles = []*os.File{writer}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, reader
}
func processHelper(mode string) {
	ready := os.NewFile(3, "ready")
	defer func() { _ = ready.Close() }()
	if mode == "leader" {
		binary, err := os.Executable()
		if err != nil {
			os.Exit(3)
		}
		child := exec.Command(binary, "-test.run=^TestOwnedCommandCancelsGroupAndKeepsCause$")
		child.Env = append(os.Environ(), "OBERTH_C6_PROCESS_TEST=child")
		child.ExtraFiles = []*os.File{ready}
		if err = child.Start(); err != nil {
			os.Exit(3)
		}
	} else {
		_, _ = fmt.Fprintf(ready, "%d %d\n", os.Getppid(), os.Getpid())
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	<-timer.C
	os.Exit(3)
}

func pinHelpers(t *testing.T, parent, child int) func() {
	t.Helper()
	leaderFD, err := unix.PidfdOpen(parent, 0)
	if err != nil {
		t.Fatal(err)
	}
	childFD, err := unix.PidfdOpen(child, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		_ = unix.PidfdSendSignal(leaderFD, syscall.SIGKILL, nil, 0)
		_ = unix.PidfdSendSignal(childFD, syscall.SIGKILL, nil, 0)
		_ = unix.Close(leaderFD)
		_ = unix.Close(childFD)
	}
}
