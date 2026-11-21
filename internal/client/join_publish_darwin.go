package client

import "golang.org/x/sys/unix"

func publishJoin(stage, dir string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, stage, unix.AT_FDCWD, dir, unix.RENAME_EXCL)
}
