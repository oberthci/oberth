//go:build unix

package client

import (
	"fmt"
	"os"
	"syscall"
)

// trustedConfigFile refuses a client file (or its directory) whose contents
// steer this user's token or commands while someone else can change it: not
// owned by the user, or writable by its group or by others.
func trustedConfigFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("client file: %w", err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by others (mode %04o); it is not trusted: chmod go-w it", path, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by this user; it is not trusted", path)
	}
	return nil
}
