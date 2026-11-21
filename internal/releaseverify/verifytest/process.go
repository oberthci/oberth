package verifytest

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func flagTimeout() string { return flag.Lookup("test.timeout").Value.String() }

func fixtureCommand(ctx context.Context, name string, args []string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 5 * time.Second
	return command
}
