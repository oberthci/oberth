package gitcache

import "golang.org/x/sys/unix"

// The typed syscall stub keeps the kernel pointer boundary in assembly rather
// than adding a Go pointer cast outside the repository's memory-hygiene boundary.
//
//go:noescape
func installGroupSignalFilter(program *unix.SockFprog) unix.Errno
