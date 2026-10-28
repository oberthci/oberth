//go:build !linux && !darwin

package gitcache

import (
	"context"
	"errors"
	"os/exec"
)

func prepareCLIWait() error {
	return errors.New("CLI Git process ownership requires Linux or Darwin")
}

func waitCLICommand(context.Context, *exec.Cmd) (error, error) {
	return prepareCLIWait(), nil // refused before Start on unsupported platforms
}
