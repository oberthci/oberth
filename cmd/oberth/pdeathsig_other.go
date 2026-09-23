//go:build !linux

package main

import "os/exec"

// setPdeathsig is a no-op on non-Linux platforms. Pdeathsig is a Linux-only
// prctl feature; the release cross-compiles for darwin but secretstore exec
// only runs in Linux pipeline containers.
func setPdeathsig(_ *exec.Cmd) {}
