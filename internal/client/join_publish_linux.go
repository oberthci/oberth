package client

import "golang.org/x/sys/unix"

func publishJoin(stage, dir string) error {
	return unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, dir, unix.RENAME_NOREPLACE)
}
