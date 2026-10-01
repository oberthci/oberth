package installer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func sealedWatchValues(raw []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate("oberth-public-watch-values", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, errors.New("cannot freeze public watch values")
	}
	f := os.NewFile(uintptr(fd), "oberth-public-watch-values")
	if _, err = f.Write(raw); err == nil {
		err = f.Chmod(0400)
	}
	if err == nil {
		_, err = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL)
	}
	if err != nil {
		_ = f.Close()
		return nil, errors.New("cannot seal public watch values")
	}
	return f, nil
}
