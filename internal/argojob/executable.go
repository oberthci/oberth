package argojob

import (
	"io"
	"os"
	"path/filepath"
)

// InstallExecutable copies only this executable, with no profile or configuration.
// The destination directory is a mounted volume, including on read-only roots.
func InstallExecutable(to string) error {
	path, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(to), ".oberth-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, src)
	if err == nil {
		err = tmp.Chmod(0555)
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), to)
	}
	return err
}
