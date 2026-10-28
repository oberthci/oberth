//go:build darwin

package gitcache

import (
	"context"
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

func prepareCLIWait() error { return nil }

// Darwin's init reaps orphaned descendants. Observe exit with kqueue rather
// than Cmd.Wait so group signaling still happens before the leader is reaped:
// the PID/PGID cannot be reused between observing exit and signaling the group.
func waitCLICommand(ctx context.Context, cmd *exec.Cmd) (error, error) {
	pid := cmd.Process.Pid
	if pid <= 0 {
		return errors.New("invalid git group leader"), nil
	}
	queue, err := unix.Kqueue()
	if err != nil {
		_ = unix.Kill(-pid, unix.SIGKILL)
		return errors.Join(err, cmd.Wait()), nil
	}
	defer func() { _ = unix.Close(queue) }()
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	_, err = unix.Kevent(queue, []unix.Kevent_t{change}, nil, nil)
	if err != nil {
		// A very short-lived child may already be a zombie by registration.
		// It is still ours and unreaped; terminating its group is safe.
		_ = unix.Kill(-pid, unix.SIGKILL)
		waitErr := cmd.Wait()
		if errors.Is(err, unix.ESRCH) {
			return waitErr, nil
		}
		return errors.Join(err, waitErr), nil
	}
	exited := make(chan error, 1)
	go func() {
		var events [1]unix.Kevent_t
		for {
			_, err := unix.Kevent(queue, nil, events[:], nil)
			if !errors.Is(err, unix.EINTR) {
				exited <- err
				return
			}
		}
	}()
	var stopErr error
	select {
	case err = <-exited:
	case <-ctx.Done():
		stopErr = context.Cause(ctx)
		_ = unix.Kill(-pid, unix.SIGKILL)
		err = <-exited
	}
	_ = unix.Kill(-pid, unix.SIGKILL)
	return errors.Join(err, cmd.Wait()), stopErr
}
