package main

import (
	"github.com/oberthci/oberth/internal/secretstore"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
	"os"
)

func readServerSigner(path, field string) (ssh.Signer, error) {
	raw, err := secretstore.ReadMountedFile(path, 64<<10, field)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, serverProblem("config", field+": invalid SSH private key", nil)
	}
	return signer, nil
}
func validateGit(path string) error {
	info, err := os.Stat(path)
	state := ""
	switch {
	case os.IsNotExist(err):
		state = "missing executable"
	case err != nil:
		state = "unreadable executable"
	case !info.Mode().IsRegular():
		state = "not a regular executable"
	case info.Size() == 0:
		state = "empty executable"
	case info.Mode().Perm()&0111 == 0 || unix.Access(path, unix.X_OK) != nil:
		state = "not executable"
	}
	if state != "" {
		return serverProblem("config", "runtime.git_binary: "+state, nil)
	}
	return nil
}
