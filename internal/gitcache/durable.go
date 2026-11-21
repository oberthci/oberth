package gitcache

import (
	"errors"
	"os"
	"path/filepath"
)

const recordTempPrefix = ".oberth-record-"

func writeDurable(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), recordTempPrefix)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
