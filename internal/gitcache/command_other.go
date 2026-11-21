//go:build !linux

package gitcache

import (
	"context"
	"os"
	"os/exec"
)

func runOwned(_ context.Context, _ *exec.Cmd) error { return refuse("process platform") }

func configureOwnedCommand(*exec.Cmd) {}
func claimRoot(*os.File) error        { return refuse("process platform") }
