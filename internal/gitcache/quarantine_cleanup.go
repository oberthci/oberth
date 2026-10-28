package gitcache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Legacy receive directories remain collectible after backend removal.
// Interrupted receives are collectible after 30 minutes. A held kernel lock
// protects even a very old active receive, including across server processes;
// process exit releases it automatically without PID reuse or lease guessing.
const receiveQuarantineMaxAge = 30 * time.Minute

func lockReceiveQuarantine(directory string, nonblocking bool) (*os.File, error) {
	// #nosec G304 -- directory is a repository-owned MkdirTemp receive directory or its same-prefix sweep entry; the fixed marker refuses symlinks.
	file, err := os.OpenFile(filepath.Join(directory, ".oberth-active"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	operation := unix.LOCK_EX
	if nonblocking {
		operation |= unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func sweepReceiveQuarantines(repository string, now time.Time) error {
	objects := filepath.Join(repository, "objects")
	entries, err := os.ReadDir(objects)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "oberth-incoming-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if now.Sub(info.ModTime()) < receiveQuarantineMaxAge {
			continue
		}
		directory := filepath.Join(objects, entry.Name())
		_, markerErr := os.Lstat(filepath.Join(directory, ".oberth-active"))
		legacy := errors.Is(markerErr, os.ErrNotExist)
		active, err := lockReceiveQuarantine(directory, true)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		// Recheck under the lock: a receive or another sweeper may have changed it.
		candidate := info
		info, err = os.Lstat(directory)
		// Older releases had no lock marker. Creating it changes the directory
		// mtime; retain the original age only for that legacy directory inode.
		if err == nil && info.IsDir() && os.SameFile(candidate, info) && (legacy || now.Sub(info.ModTime()) >= receiveQuarantineMaxAge) {
			err = os.RemoveAll(directory)
		}
		failures = errors.Join(failures, err, active.Close())
	}
	return failures
}
