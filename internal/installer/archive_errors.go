package installer

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

func backupFailure(code string, err error, path string) error {
	if err == nil {
		return nil
	}
	return described(code, err, path+"\n"+backupCause(err, err.Error()))
}

func backupCause(err error, text string) string {
	var file *os.PathError
	if errors.As(err, &file) {
		text = strings.ReplaceAll(text, file.Error(), file.Op+": "+file.Err.Error())
	}
	var link *os.LinkError
	if errors.As(err, &link) {
		text = strings.ReplaceAll(text, link.Error(), link.Op+": "+link.Err.Error())
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			text = backupCause(child, text)
		}
	} else if child := errors.Unwrap(err); child != nil {
		text = backupCause(child, text)
	}
	return text
}

func backupLinkFailure(err error, path string) error {
	code := "backup-publish"
	switch {
	case errors.Is(err, os.ErrExist):
		code = "restore-exists"
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.EXDEV), errors.Is(err, syscall.ENOSYS):
		code = "backup-hardlink"
	}
	return backupFailure(code, err, path)
}

func backupDirectorySyncFailure(err error, path string) error {
	return backupFailure("backup-directory-sync", err, path)
}
