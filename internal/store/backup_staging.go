package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Matches the in-pod operation deadline. Age alone never authorizes removing
// a live snapshot: the kernel lock is held through verification and streaming.
const onlineBackupStagingMaxAge = 10 * time.Minute

const onlineBackupStagingPrefix = "oberth-online-backup-"

func newOnlineBackupStaging(parent string) (string, func() error, error) {
	if err := sweepOnlineBackupStaging(parent, time.Now()); err != nil {
		return "", nil, fmt.Errorf("clean interrupted database snapshots: %w", err)
	}
	dir, err := os.MkdirTemp(parent, onlineBackupStagingPrefix)
	if err != nil {
		return "", nil, fmt.Errorf("stage database snapshot on database volume: %w", err)
	}
	active, err := lockOnlineBackupStaging(dir)
	if err != nil {
		return "", nil, errors.Join(err, os.RemoveAll(dir))
	}
	return dir, func() error { return errors.Join(os.RemoveAll(dir), active.Close()) }, nil
}

func onlineBackupStagingOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) //nolint:gosec // euid is an OS uid, represented by uint32 in Stat_t.
}

func lockOnlineBackupStaging(dir string) (*os.File, error) {
	// #nosec G304 -- fixed marker in a private staging directory; refuse symlinks and special files.
	file, err := os.OpenFile(filepath.Join(dir, ".active"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || !onlineBackupStagingOwned(info)) {
		err = errors.New("database snapshot lock ownership or type refused")
	}
	if err == nil {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

// SIGKILL cannot run defers. The next backup reclaims only old, same-euid,
// same-prefix directories whose active lock is free. Directory symlinks are
// never traversed. A still-running backup remains protected even past its TTL.
func sweepOnlineBackupStaging(parent string, now time.Time) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), onlineBackupStagingPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if !onlineBackupStagingOwned(info) || now.Sub(info.ModTime()) <= onlineBackupStagingMaxAge {
			continue
		}
		dir := filepath.Join(parent, entry.Name())
		active, err := lockOnlineBackupStaging(dir)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		current, err := os.Lstat(dir)
		if err == nil && current.IsDir() && os.SameFile(info, current) && onlineBackupStagingOwned(current) {
			err = os.RemoveAll(dir)
		}
		if err = errors.Join(err, active.Close()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
