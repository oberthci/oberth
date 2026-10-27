//go:build linux

package gitcache

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

func shadowHelperPriorityOK() bool {
	raw, err := os.ReadFile("/proc/self/oom_score_adj")
	if err != nil || strings.TrimSpace(string(raw)) != "1000" {
		return false
	}
	// Linux Getpriority returns 20-nice: every thread must have nice >=10.
	// Starting via nice must cover runtime threads, not only the calling thread.
	threads, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return false
	}
	for _, thread := range threads {
		tid, err := strconv.Atoi(thread.Name())
		if err != nil {
			return false
		}
		priority, err := syscall.Getpriority(syscall.PRIO_PROCESS, tid)
		if err != nil || priority < 1 || priority > 10 {
			return false
		}
	}
	return len(threads) > 0
}
