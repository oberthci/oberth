package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
)

// Server state permits access only to the server owner and its own Pod group.
// New files remain private even when the volume adds group read/write access.
func checkServerState(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !safeServerState(info, directory) {
		problem := publicerr.New(publicerr.Forbidden, "Server state permissions are unsafe.", "Restore the server's ownership and group; remove other access and group execute access to files.", nil)
		problem.Context = []string{"File: " + path}
		return problem
	}
	return nil
}

func safeServerState(info os.FileInfo, directory bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	if info.Mode().Perm()&0070 != 0 && stat.Gid != uint32(os.Getegid()) {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode().Perm()&0007 == 0
	}
	return info.Mode().IsRegular() && info.Mode().Perm()&0017 == 0
}

func readServerState(path string) ([]byte, error) {
	if err := checkServerState(filepath.Dir(path), true); err != nil {
		return nil, err
	}
	if err := checkServerState(path, false); err != nil {
		return nil, err
	}
	return secretstore.ReadMountedFile(path, 64<<10, "server identity")
}

func writeServerState(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := checkServerState(dir, true); err != nil {
		return err
	}
	if err := checkServerState(path, false); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(dir, ".oberth-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
