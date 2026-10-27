//go:build !linux

package gitcache

import "syscall"

func shadowHelperPriorityOK() bool {
	priority, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	return err == nil && priority >= 10
}
