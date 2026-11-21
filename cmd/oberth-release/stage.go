package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func stagePublication(release *releaseverify.Release, dir string) error {
	for _, name := range publishedNames() {
		input, err := release.Open(name)
		if err != nil {
			return err
		}
		if err := stageFile(input, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	sums, bundle := release.Manifest()
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), sums, 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS.sigstore.json"), bundle, 0600)
}
func stageFile(input io.ReadCloser, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return errors.Join(err, input.Close())
	}
	_, copyErr := io.Copy(file, input)
	return errors.Join(copyErr, input.Close(), file.Close())
}
