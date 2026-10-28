package store

import (
	"errors"
	"fmt"
	"math"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrBackupSpace is a refusal, not an old-server capability error. In
// particular a restore preview must not retry or bypass it with another copy.
var ErrBackupSpace = errors.New("backup space refused")

const backupSpaceMinimumReserve int64 = 256 << 20
const backupSpaceMetadata int64 = 64 << 10

type backupSpaceGuard struct {
	volume, snapshot string
	size, reserve    int64
	free             func(string) (int64, error)
}

func (g backupSpaceGuard) check() error {
	remaining := g.size
	if g.snapshot != "" {
		info, err := os.Stat(g.snapshot)
		if err != nil {
			return err
		}
		// Size alone can include sparse/unwritten pages. Only actual allocated
		// blocks reduce the remaining space required, including SQLite's dirty
		// destination cache which may not have reached disk yet.
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Blocks < 0 {
			return fmt.Errorf("%w: cannot inventory allocated snapshot blocks", ErrBackupSpace)
		}
		if stat.Blocks >= (g.size+511)/512 {
			remaining = 0
		} else {
			remaining -= stat.Blocks * 512
		}
	}
	needed := remaining + g.reserve + backupSpaceMetadata
	free, err := g.free(g.volume)
	if err != nil {
		return fmt.Errorf("%w: inspect free space on volume %q: %w", ErrBackupSpace, g.volume, err)
	}
	if free < needed {
		return fmt.Errorf("%w: volume %q has %d bytes free, needs %d bytes (%d remaining snapshot + %d reserve + %d metadata); grow/free the volume or copy the source read-only to a larger workstation volume", ErrBackupSpace, g.volume, free, needed, remaining, g.reserve, backupSpaceMetadata)
	}
	return nil
}

func backupVolumeFreeBytes(path string) (int64, error) {
	return backupVolumeFreeBytesWithStatfs(path, unix.Statfs)
}

func backupVolumeFreeBytesWithStatfs(path string, statfs func(string, *unix.Statfs_t) error) (int64, error) {
	var stat unix.Statfs_t
	if err := statfs(path, &stat); err != nil {
		return 0, err
	}
	blockSize := int64(stat.Bsize) //nolint:unconvert // uint32 on darwin, int64 on linux.
	if blockSize <= 0 {
		return 0, errors.New("invalid filesystem block size")
	}
	if stat.Bavail > uint64(math.MaxInt64/blockSize) { //nolint:gosec // blockSize is positive, so the quotient is in [0, MaxInt64].
		// Some filesystems encode a negative available-block count in the
		// unsigned ABI field. Neither that nor an overflowing inventory is
		// evidence of space: refuse instead of saturating to a huge budget.
		return 0, errors.New("filesystem available bytes are negative or overflow the supported bound")
	}
	return int64(stat.Bavail) * blockSize, nil //nolint:gosec // bounded above before conversion and multiplication.
}

// A directory flock serializes cooperating snapshotters without allocating a
// new lock file on a nearly full volume. Hold it through archive emission, so
// a second snapshot cannot spend the first one's measured reserve.
func lockOnlineBackupVolume(parent string) (*os.File, error) {
	file, err := os.Open(parent) // #nosec G304 -- parent of validated operator database path.
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("database snapshot volume is busy or cannot be locked: %w", err), file.Close())
	}
	return file, nil
}
