package gitcache

import (
	"os"
	"syscall"
	"time"
)

func hookChangeTime(info os.FileInfo) time.Time {
	stat := info.Sys().(*syscall.Stat_t)
	return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)
}
