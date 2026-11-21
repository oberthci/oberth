package client

import (
	"errors"
	"io/fs"
	"os"

	"github.com/oberthci/oberth/internal/publicerr"
)

type fileError struct {
	public *publicerr.Error
	path   string
}

func (e *fileError) Reason() string { return e.public.Reason() }
func (e *fileError) Remedy() string { return e.public.Remedy() }
func (e *fileError) Unwrap() error  { return e.public }
func (e *fileError) Path() string   { return e.path }
func (e *fileError) Error() string  { return e.public.Error() + "\nFile: " + e.path }

func fileFailure(path, operation string, cause error) error {
	key := operation
	if errors.Is(cause, fs.ErrNotExist) {
		key = "missing"
	}
	m, ok := fileMessages()[key]
	if !ok {
		m = fileMessages()["unsafe"]
	}
	return &fileError{publicerr.New(publicerr.Unavailable, m[0], m[1], cause), path}
}

func permissionMask(private bool) os.FileMode {
	if private {
		return 077
	}
	return 022
}
