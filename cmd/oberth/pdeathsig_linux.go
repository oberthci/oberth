//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// setPdeathsig ties the child process lifetime to the wrapper process: when
// the parent exits (for any reason, including SIGKILL), the kernel delivers
// SIGKILL to the child. Without this, a wrapper crash leaves the child
// running with credential environment stripped but no redaction on
// stdout/stderr — an unmonitored process with potential access to secrets
// still in its address space.
func setPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
