package sshserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/gitssh"
	"k8s.io/utils/clock"
)

// The qualification payload dispatches the real SSH shim from its helper name.
func baseSSHInvocation() (int, bool) {
	if filepath.Base(os.Args[0]) != "oberth-git-ssh" {
		return 0, false
	}
	config, err := gitssh.Parse(os.Args[1:])
	if err == nil {
		config, err = gitssh.Configure(os.Getenv(gitssh.ConfigEnvironment), config)
	}
	config.Now = clock.RealClock{}.Now
	if err == nil {
		err = gitssh.Run(context.Background(), config, gitssh.IO{Input: os.Stdin, Output: os.Stdout, Diagnostics: os.Stderr})
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1, true
	}
	return 0, true
}
