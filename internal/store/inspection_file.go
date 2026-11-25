package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/oberthci/oberth/internal/stateport"
)

// Remove the private stage before extracting any decrypted bytes. The open file
// remains available to SQLite's read-only VFS and is reclaimed on process death.
func anonymousInspection(parent string, options stateport.Options) (_ *os.File, _ func() error, resultErr error) {
	limit, _ := options.Limit()
	var stats syscall.Statfs_t
	if syscall.Statfs("/dev/shm", &stats) == nil && uint64(stats.Bsize)*stats.Bavail > uint64(limit+(128<<10)) {
		if err := sweepStaging(parent); err != nil {
			return nil, nil, err
		}
		parent = "/dev/shm"
	}
	stage, removeStage, err := newStage(parent)
	if err != nil {
		return nil, nil, err
	}
	var file *os.File
	remove := removeStage
	defer func() {
		if resultErr != nil {
			if file != nil {
				resultErr = errors.Join(resultErr, file.Close())
			}
			if remove != nil {
				resultErr = errors.Join(resultErr, remove())
			}
		}
	}()
	path := filepath.Join(stage, "oberth.sqlite")
	file, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, nil, databaseError(err)
	}
	remove = nil
	if err = removeStage(); err != nil {
		return nil, nil, databaseError(err)
	}
	return file, file.Close, nil
}
