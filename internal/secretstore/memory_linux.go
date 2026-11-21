//go:build linux

package secretstore

import "golang.org/x/sys/unix"

func memoryDirectory(path string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return refuse("delivery", err)
	}
	if st.Type != unix.TMPFS_MAGIC {
		return refuse("delivery", nil)
	}
	return nil
}
