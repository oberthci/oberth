package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const encryption = "age v1; X25519"

func refused() error { return recoveryError(bindingInvalid, nil) }

func fileDigest(ctx context.Context, path string, progress func()) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, contextReader{ctx: ctx, Reader: file, progress: progress})
	return hex.EncodeToString(hash.Sum(nil)), errors.Join(copyErr, file.Close())
}

func cleanPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "?\x00#")
}

func regular(path string, maximum int64, private bool) error {
	if !cleanPath(path) {
		return refused()
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum {
		return refused()
	}
	if private && info.Mode().Perm()&0077 != 0 {
		result := recoveryError(identityPublic, nil)
		result.Detail = fmt.Sprintf("File mode: %04o.", info.Mode().Perm())
		return result
	}
	return nil
}

func exclusiveOutput(path string) (*os.File, error) {
	if !cleanPath(path) {
		return nil, refused()
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, recoveryError(outputPublic, nil)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
}

func syncParent(path string) error {
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func recoveryParent(c config) (string, error) {
	if c.mode != "restore" {
		return filepath.Dir(c.input), nil
	}
	if !cleanPath(c.output) {
		return "", refused()
	}
	if _, err := os.Lstat(c.output); err == nil {
		return "", recoveryError(fileExists, nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Dir(c.output), nil
}

func syncOutput(file *os.File) error { return errors.Join(file.Sync(), file.Close()) }
