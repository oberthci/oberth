package service

import (
	"sync"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// diskFloorCheck provides an opt-in disk-space admission hold. When the
// floor is positive and the observed free bytes on the configured path are
// below it, canGrant returns false and the admission observation reports
// the hold reason.
//
// The check uses statfs, which reads the kernel's in-memory superblock
// and returns in sub-microsecond time. The result is cached for a brief
// window (default 5 s) so the grant loop never issues redundant syscalls.
//
// Issue #673: the incident on 2026-09-27 had DiskPressure evict Oberth
// because the weighted admission gate had no free-byte input.
type diskFloorCheck struct {
	floorBytes int64
	path       string

	mu         sync.Mutex
	cachedFree int64
	cacheTime  time.Time
	cacheTTL   time.Duration
}

// newDiskFloorCheck builds a check for the given path. A floorBytes of zero
// means disabled: freeAboveFloor always returns true.
func newDiskFloorCheck(path string, floorBytes int64) *diskFloorCheck {
	ttl := 5 * time.Second
	return &diskFloorCheck{
		floorBytes: floorBytes,
		path:       path,
		cacheTTL:   ttl,
	}
}

// freeAboveFloor reports whether the filesystem has enough free bytes.
// Returns true when the check is disabled (floorBytes == 0).
func (d *diskFloorCheck) freeAboveFloor() bool {
	if d == nil || d.floorBytes <= 0 {
		return true
	}
	free := d.freeBytes()
	return free >= d.floorBytes
}

// freeBytes returns the cached or freshly observed free bytes.
func (d *diskFloorCheck) freeBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.cacheTime) < d.cacheTTL {
		return d.cachedFree
	}
	d.cachedFree = statfsFreeBytes(d.path)
	d.cacheTime = time.Now()
	return d.cachedFree
}

// observation returns the disk floor state for scheduling display.
// Returns nil when the check is disabled.
func (d *diskFloorCheck) observation() *model.DiskFloorObservation {
	if d == nil || d.floorBytes <= 0 {
		return nil
	}
	free := d.freeBytes()
	return &model.DiskFloorObservation{
		FloorBytes: d.floorBytes,
		FreeBytes:  free,
		BelowFloor: free < d.floorBytes,
	}
}

// statfsFreeBytes returns the available free bytes for unprivileged users
// on the filesystem containing path. Returns 0 on error.
func statfsFreeBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	// Bavail is the free blocks available to unprivileged users.
	// Bavail is uint64; Bsize is int64 on linux but uint32 on darwin.
	// Cast both to int64 so the arithmetic compiles on every GOOS.
	// Overflow would require >8 EiB free, which is not a real filesystem.
	return int64(stat.Bavail) * int64(stat.Bsize) //nolint:gosec,unconvert // int64 cast required: Bsize is uint32 on darwin
}
