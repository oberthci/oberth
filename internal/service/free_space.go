package service

import "syscall"

// statfsFreeBytes returns the available free bytes for unprivileged users
// on the filesystem containing path. Returns 0 on error.
func statfsFreeBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	// Bavail is the free blocks available to unprivileged users.
	// Bavail is uint64; Bsize is int64 on linux but uint32 on darwin.
	// Cast both to int64 so the arithmetic compiles on every GOOS.
	// Overflow would require >8 EiB free, which is not a real filesystem.
	return int64(stat.Bavail) * int64(stat.Bsize) //nolint:gosec,unconvert // int64 cast required: Bsize is uint32 on darwin
}
