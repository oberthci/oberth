package kubeconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type copyOwner struct {
	Format    int
	Name      string
	CreatedAt time.Time
}

func copyRootPath() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "oberth", "kubeconfig-sessions"), nil
}

func privateRoot() (string, error) {
	path, err := copyRootPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return "", err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !ownedMode(st, 0700) || !st.IsDir() {
		return "", errors.New("private kubeconfig root ownership or permissions are unsafe")
	}
	return path, nil
}
func ownedMode(st os.FileInfo, mode os.FileMode) bool {
	owner, ok := st.Sys().(*syscall.Stat_t)
	return ok && int64(owner.Uid) == int64(os.Getuid()) && st.Mode().Perm() == mode && st.Mode()&os.ModeSymlink == 0
}

// Sweep reclaims unlocked, marked copies created no later than this startup.
// Live commands hold an exclusive kernel lock, released even by SIGKILL/crash.
// No fallback deletes unmarked, malformed, future-dated or foreign objects.
func Sweep() (resultErr error) {
	path, err := copyRootPath()
	if err != nil {
		return nil
	}
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() || !ownedMode(st, 0700) {
		return errors.New("private kubeconfig root ownership or permissions are unsafe")
	}

	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "copy-") {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if !info.IsDir() || !ownedMode(info, 0700) {
			continue
		}
		dir, err := root.OpenRoot(entry.Name())
		if err != nil {
			return err
		}
		err = sweepCopy(root, dir, entry.Name(), info, now)
		err = errors.Join(err, dir.Close())
		if err != nil {
			return fmt.Errorf("reclaim private kubeconfig copy %s: %w", entry.Name(), err)
		}
	}
	return nil
}
func sweepCopy(root, dir *os.Root, name string, info os.FileInfo, now time.Time) (resultErr error) {
	initial, err := dir.Lstat(".owner")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !initial.Mode().IsRegular() || !ownedMode(initial, 0600) {
		return errors.New("ownership marker is unsafe; preserved")
	}
	marker, err := dir.OpenFile(".owner", os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, marker.Close()) }()
	st, err := marker.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(initial, st) || !st.Mode().IsRegular() || !ownedMode(st, 0600) || st.Size() > 4096 {
		return errors.New("ownership marker is unsafe; preserved")
	}
	if err := unix.Flock(int(marker.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	var owner copyOwner
	decoder := json.NewDecoder(io.LimitReader(marker, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&owner); err != nil {
		return errors.New("ownership marker is unreadable; preserved")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("ownership marker has trailing data; preserved")
	}
	if owner.Format != 1 || owner.Name != name || owner.CreatedAt.IsZero() || owner.CreatedAt.After(now) {
		return errors.New("ownership or age is unverified; preserved")
	}
	current, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return errors.New("directory replaced; preserved")
	}
	return root.RemoveAll(name)
}

func newPrivateCopy() (string, *os.File, func() error, error) {
	noop := func() error { return nil }
	if err := Sweep(); err != nil {
		return "", nil, noop, err
	}
	path, err := privateRoot()
	if err != nil {
		return "", nil, noop, err
	}
	directory, err := os.MkdirTemp(path, "copy-")
	if err != nil {
		return "", nil, noop, err
	}
	// Publish a complete, locked marker atomically before writing credentials.
	// Other startups ignore the unfinished, unmarked directory.
	dir, err := os.OpenRoot(directory)
	if err != nil {
		return "", nil, noop, errors.Join(err, os.RemoveAll(directory))
	}
	marker, err := dir.OpenFile(".owner.pending", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		var markerErr error
		if marker != nil {
			markerErr = marker.Close()
		}
		return "", nil, noop, errors.Join(err, closeErr, markerErr, os.RemoveAll(directory))
	}
	if err = unix.Flock(int(marker.Fd()), unix.LOCK_EX); err == nil {
		err = json.NewEncoder(marker).Encode(copyOwner{Format: 1, Name: filepath.Base(directory), CreatedAt: time.Now()})
	}
	if err != nil {
		return "", nil, noop, errors.Join(err, marker.Close(), os.RemoveAll(directory))
	}
	if err := os.Rename(filepath.Join(directory, ".owner.pending"), filepath.Join(directory, ".owner")); err != nil {
		return "", nil, noop, errors.Join(err, marker.Close(), os.RemoveAll(directory))
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", nil, noop, errors.Join(err, marker.Close(), os.RemoveAll(directory))
	}
	var once sync.Once
	var cleanupErr error
	cleanup := func() error {
		once.Do(func() {
			current, err := os.Lstat(directory)
			if errors.Is(err, os.ErrNotExist) {
				cleanupErr = marker.Close()
				return
			}
			if err == nil && !os.SameFile(info, current) {
				err = errors.New("private kubeconfig directory replaced; preserved")
			}
			if err == nil {
				err = os.RemoveAll(directory)
			}
			cleanupErr = errors.Join(err, marker.Close())
			if cleanupErr != nil {
				cleanupErr = fmt.Errorf("private kubeconfig cleanup failed: %w", cleanupErr)
			}
		})
		return cleanupErr
	}
	return directory, marker, cleanup, nil
}
