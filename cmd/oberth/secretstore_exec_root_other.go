//go:build !linux

package main

import (
	"errors"
	"os"
)

func defaultSecretExecFstatfs(int) (int64, error) {
	return 0, errors.New("secretstore exec requires Linux tmpfs")
}

func prepareSecretExecRoot(string) (*os.File, error) {
	return nil, errors.New("secretstore exec requires Linux tmpfs")
}

func checkSecretExecRoot(*os.File) error {
	return errors.New("secretstore exec requires Linux tmpfs")
}

func writeSecretTreeFD(*os.File, string, map[string][]byte) error {
	return errors.New("secretstore exec requires Linux tmpfs")
}
