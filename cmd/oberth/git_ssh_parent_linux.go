//go:build linux

package main

import (
	"errors"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// The transport has no children. Binding its lifetime to Git also handles a
// parent killed before it can run Git's normal child cleanup handlers.
func bindGitSSHParent() (func(), error) {
	runtime.LockOSThread()
	parent := os.Getppid()
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	if parent <= 1 || os.Getppid() != parent {
		runtime.UnlockOSThread()
		return nil, errors.New("git parent exited")
	}
	return runtime.UnlockOSThread, nil
}
