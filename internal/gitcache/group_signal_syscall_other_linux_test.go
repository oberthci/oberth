//go:build linux && !amd64 && !arm64

package gitcache

import "golang.org/x/sys/unix"

func installGroupSignalFilter(*unix.SockFprog) unix.Errno { return unix.ENOSYS }
