package client

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type selectionDirectory struct {
	path string
	info os.FileInfo
}

// Only approved selection reservation calls this; existing paths are never changed.
func createSelectionDirectories(path string) ([]selectionDirectory, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fileFailure(path, "directory", err)
	}
	created, err := createSelectionDirectories(filepath.Dir(path))
	if err != nil {
		return created, err
	}
	if err = os.Mkdir(path, 0700); errors.Is(err, os.ErrExist) {
		return created, nil
	}
	if err != nil {
		return created, fileFailure(path, "directory", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return created, fileFailure(path, "directory", err)
	}
	return append(created, selectionDirectory{path: path, info: info}), nil
}

func removeSelectionDirectories(created []selectionDirectory) error {
	var result error
	for i := len(created) - 1; i >= 0; i-- {
		dir := created[i]
		info, err := os.Lstat(dir.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil && (!info.IsDir() || !os.SameFile(info, dir.info)) {
			continue
		}
		if err == nil {
			err = unix.Rmdir(dir.path)
		}
		if err != nil && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST) && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fileFailure(dir.path, "directory", err))
		}
	}
	return result
}
