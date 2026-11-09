//go:build unix

package holdfs

import "syscall"

const noFollow = syscall.O_NOFOLLOW
