package argoworkflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestControllerCommandDeadlineKillsDescendants(t *testing.T) {
	if mode := os.Getenv("OBERTH_CONTROLLER_PROCESS_TEST"); mode != "" {
		controllerProcessHelper(t, mode)
		return
	}
	ctx, cancel := context.WithTimeout(controllerContext(t), 5*time.Second)
	defer cancel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = reader.Close() })
	defer stop()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestControllerCommandDeadlineKillsDescendants$", "-test.timeout=30s")
	command.Env = append(os.Environ(), "OBERTH_CONTROLLER_PROCESS_TEST=leader")
	command.ExtraFiles = []*os.File{writer}
	finished := make(chan struct{})
	var commandErr error
	go func() { _, commandErr = controllerOutput(command); close(finished) }()
	defer func() { cancel(); <-finished }()
	var parent, child int
	if _, err = fmt.Fscanf(reader, "%d %d\n", &parent, &child); err != nil {
		t.Fatal(err)
	}
	fds := pinControllerProcesses(t, []int{parent, child})
	<-finished
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || commandErr == nil {
		t.Fatal("command did not stop at its deadline")
	}
	controllerProcessesExited(t, fds)
}

func controllerProcessHelper(t *testing.T, mode string) {
	t.Helper()
	ready := os.NewFile(3, "ready")
	defer func() { _ = ready.Close() }()
	if mode == "leader" {
		child := exec.Command(os.Args[0], "-test.run=^TestControllerCommandDeadlineKillsDescendants$", "-test.timeout=30s")
		child.Env = append(os.Environ(), "OBERTH_CONTROLLER_PROCESS_TEST=child")
		child.ExtraFiles = []*os.File{ready}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := fmt.Fprintf(ready, "%d %d\n", os.Getppid(), os.Getpid()); err != nil {
			t.Fatal(err)
		}
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	<-timer.C
}

func pinControllerProcesses(t *testing.T, pids []int) []unix.PollFd {
	t.Helper()
	fds := make([]unix.PollFd, 0, len(pids))
	for _, pid := range pids {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0)
			_ = unix.Close(fd)
		})
		fds = append(fds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
	}
	return fds
}

func controllerProcessesExited(t *testing.T, pending []unix.PollFd) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for len(pending) > 0 {
		if ctx.Err() != nil {
			t.Fatal("command deadline left a running descendant")
		}
		_, err := unix.Poll(pending, 100)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		remaining := pending[:0]
		for _, fd := range pending {
			if fd.Revents&unix.POLLIN == 0 {
				remaining = append(remaining, fd)
			}
		}
		pending = remaining
	}
}
