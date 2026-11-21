package store

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/oberthci/oberth/internal/publicerr"
	"golang.org/x/sys/unix"
)

const stagePrefix = ".oberth-state-"

func lockParent(parent string) (*os.File, error) {
	dir, err := os.Open(parent)
	if err != nil {
		return nil, databaseError(err)
	}
	if err := unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(refuse(stageBusy, err), dir.Close())
	}
	return dir, nil
}

func newStage(parent string) (string, func() error, error) {
	if err := sweepStaging(parent); err != nil {
		return "", nil, err
	}
	stage, err := os.MkdirTemp(parent, stagePrefix)
	if err != nil {
		return "", nil, databaseError(err)
	}
	lock, err := lockParent(stage)
	if err != nil {
		return "", nil, errors.Join(err, os.RemoveAll(stage))
	}
	if err := os.WriteFile(filepath.Join(stage, "owner"), []byte("oberth.state-stage/v1\n"), 0600); err != nil {
		return "", nil, errors.Join(err, os.RemoveAll(stage), lock.Close())
	}
	return stage, func() error { return errors.Join(os.RemoveAll(stage), lock.Close()) }, nil
}

func sweepStaging(parent string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return databaseError(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), stagePrefix) {
			continue
		}
		path := filepath.Join(parent, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return databaseError(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ownedStage(entry, info, stat, ok) {
			continue
		}
		marker, err := os.ReadFile(filepath.Join(path, "owner"))
		if err != nil || string(marker) != "oberth.state-stage/v1\n" {
			continue
		}
		lock, err := lockParent(path)
		if err != nil {
			continue
		}
		err = errors.Join(os.RemoveAll(path), lock.Close())
		if err != nil {
			return databaseError(err)
		}
	}
	return nil
}

type spaceGuard struct {
	parent, path string
	size         int64
	free         func(string) (int64, error)
}

func (g spaceGuard) check() error {
	remaining := g.size
	info, err := os.Stat(g.path)
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Blocks < 0 {
			return refuse(unavailable, nil)
		}
		remaining -= min(remaining, stat.Blocks*512)
	} else if !errors.Is(err, os.ErrNotExist) {
		return databaseError(err)
	}
	needed := remaining + max(int64(256<<20), g.size/4) + (64 << 10)
	free, err := g.free(g.parent)
	if err != nil {
		return databaseError(err)
	}
	if free < needed {
		return measuredRefusal(space, fmt.Sprintf("Available %d bytes; required %d bytes.", free, needed))
	}
	return nil
}

func freeBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	size := int64(stat.Bsize)
	if size <= 0 || stat.Bavail > uint64(math.MaxInt64/size) {
		return 0, refuse(unavailable, nil)
	}
	return int64(stat.Bavail) * size, nil
}

func sizeError(size, limit int64) error {
	cause := &SizeLimitError{Size: size, Limit: limit}
	code, reason, remedy := message(tooLarge)
	err := publicerr.New(code, reason, remedy, cause)
	err.Detail = cause.Error()
	return err
}

func ownedStage(entry os.DirEntry, info os.FileInfo, stat *syscall.Stat_t, ok bool) bool {
	return entry.IsDir() && info.Mode().Perm() == 0700 && ok && int(stat.Uid) == os.Geteuid()
}

// SizeLimitError identifies a measured snapshot bound independently of its message.
type SizeLimitError struct{ Size, Limit int64 }

func (e *SizeLimitError) Error() string {
	return fmt.Sprintf("Measured %d bytes; limit %d bytes.", e.Size, e.Limit)
}
