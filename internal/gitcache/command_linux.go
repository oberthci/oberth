package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// The unreaped leader pins the group ID until cancellation and cleanup finish.
func runOwned(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() {
		var info unix.Siginfo
		for {
			err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if !errors.Is(err, syscall.EINTR) {
				exited <- err
				return
			}
		}
	}()
	var cause error
	select {
	case cause = <-exited:
	case <-ctx.Done():
		cause = context.Cause(ctx)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cause = errors.Join(cause, <-exited)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return errors.Join(cause, cmd.Wait(), reapGroup(cmd.Process.Pid))
}

func reapGroup(group int) error {
	for {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(-group, &status, 0, nil)
		if errors.Is(err, syscall.ECHILD) {
			return nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
	}
}

func configureOwnedCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
func claimRoot(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		detail := fmt.Errorf("source cache is already owned by another server process: %w", err)
		return unavailable("open source cache", detail)
	}
	return nil
}
