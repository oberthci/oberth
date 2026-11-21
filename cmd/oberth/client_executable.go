package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The installed link stays fixed while the fetcher replaces versioned binaries.
func (a application) clientExecutable() (string, error) {
	executable := a.executable
	if executable == nil {
		executable = os.Executable
	}
	path, err := executable()
	if err == nil {
		path, err = filepath.EvalSymlinks(path)
	}
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil || !strings.HasPrefix(filepath.Base(path), "oberth-v") {
		return path, err
	}
	stable := filepath.Join(filepath.Dir(path), "oberth")
	target, err := filepath.EvalSymlinks(stable)
	if err != nil {
		return "", err
	}
	if target != path {
		return "", errors.New("installed link does not select the running executable")
	}
	return stable, nil
}

func (s *flowSession) installClientGitShim(path, executable string) error {
	target, err := os.Readlink(path)
	if err == nil && target == executable {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return clientShimFailure(path, err)
	}
	temp := filepath.Join(filepath.Dir(path), ".git-ssh-"+rand.Text())
	if err = os.Symlink(executable, temp); err != nil {
		return clientShimFailure(path, err)
	}
	defer func() { _ = os.Remove(temp) }()
	if err = os.Rename(temp, path); err != nil {
		return clientShimFailure(path, err)
	}
	if target != "" && s.app.diagnostic != nil {
		_, err = fmt.Fprintln(s.app.diagnostic, "Repaired the profile's Git helper.")
	}
	return err
}

func clientShimFailure(path string, cause error) error {
	problem := flowError("client_shim", cause)
	problem.Context = []string{"Git helper: " + path}
	return problem
}
