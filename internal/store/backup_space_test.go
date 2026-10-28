package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
	"modernc.org/sqlite"

	"github.com/oberthci/oberth/internal/model"
)

func backupLogicalBytes(t *testing.T, db *Store) int64 {
	t.Helper()
	var pages, size int64
	if err := db.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		t.Fatal(err)
	}
	return pages * size
}

func noOnlineBackupStaging(t *testing.T, path string) {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(path), onlineBackupStagingPrefix+"*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("snapshot staging retained", entries, err)
	}
}

func TestOnlineBackupSpaceRefusesBeforeStaging(t *testing.T) {
	for _, mode := range []string{"insufficient", "statfs-error"} {
		t.Run(mode, func(t *testing.T) {
			path, db := backupFixture(t)
			size := backupLogicalBytes(t, db)
			free := size + backupSpaceMinimumReserve + backupSpaceMetadata - 1
			var preflightErr error
			_, _, err := snapshotOnlineDatabaseWithSpace(t.Context(), path, DefaultBackupMaxBytes,
				func(*sqlite.Backup) (bool, error) { return false, errors.New("copy started despite space refusal") },
				func(volume string) (int64, error) {
					if volume != filepath.Dir(path) {
						preflightErr = fmt.Errorf("checked a different volume %q", volume)
					}
					dirs, err := filepath.Glob(filepath.Join(filepath.Dir(path), onlineBackupStagingPrefix+"*"))
					if err != nil || len(dirs) != 0 {
						preflightErr = errors.Join(fmt.Errorf("preflight already has staging: %v", dirs), err)
					}
					if mode == "statfs-error" {
						return 0, errors.New("fixture cannot read statfs")
					}
					return free, nil
				})
			if preflightErr != nil {
				t.Fatal(preflightErr)
			}
			if !errors.Is(err, ErrBackupSpace) || !strings.Contains(err.Error(), filepath.Dir(path)) {
				t.Fatal(err)
			}
			if mode == "insufficient" && (!strings.Contains(err.Error(), fmt.Sprint(free)) || !strings.Contains(err.Error(), fmt.Sprint(free+1))) {
				t.Fatal("refusal lacks available and required bytes", err)
			}
			noOnlineBackupStaging(t, path)
			if _, err := db.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: "live", Action: "after-refusal", ResourceType: "fixture", ResourceID: mode}); err != nil {
				t.Fatal("live database cannot write after refusal", err)
			}
		})
	}
}

func TestOnlineBackupSpaceRechecksAndKeepsLiveHeadroom(t *testing.T) {
	path, db := backupFixture(t)
	if _, err := db.db.Exec("CREATE TABLE pad(data BLOB); INSERT INTO pad VALUES(zeroblob(8388608))"); err != nil {
		t.Fatal(err)
	}
	size := backupLogicalBytes(t, db)
	// A simulated quota-backed filesystem reports actual allocated snapshot
	// blocks against this budget. Workspace growth is injected after a real
	// SQLite step; the next copy step must never start.
	capacity := size + backupSpaceMinimumReserve + backupSpaceMetadata
	steps, checks := 0, 0
	reserveSpent := false
	var used int64
	free := func(string) (int64, error) {
		checks++
		used = 0
		entries, err := filepath.Glob(filepath.Join(filepath.Dir(path), onlineBackupStagingPrefix+"*", "oberth.sqlite"))
		if err != nil {
			return 0, err
		}
		for _, entry := range entries {
			info, err := os.Stat(entry)
			if err != nil {
				return 0, err
			}
			used += info.Sys().(*syscall.Stat_t).Blocks * 512
		}
		available := capacity - used
		if available < backupSpaceMinimumReserve {
			reserveSpent = true
		}
		return available, nil
	}
	_, _, err := snapshotOnlineDatabaseWithSpace(t.Context(), path, DefaultBackupMaxBytes, func(backup *sqlite.Backup) (bool, error) {
		steps++
		more, err := backup.Step(128)
		if !more || err != nil {
			return more, errors.Join(fmt.Errorf("fixture did not exercise an intermediate step: more=%t", more), err)
		}
		// The workspace consumes half of the not-yet-copied budget, while
		// leaving the reserve itself untouched. Continuing to copy is unsafe.
		capacity -= size / 2
		return more, err
	}, free)
	if !errors.Is(err, ErrBackupSpace) || steps != 1 || checks < 3 || reserveSpent {
		t.Fatal("copy did not refuse pressure before a second step", steps, checks, err)
	}
	noOnlineBackupStaging(t, path)
	if remaining, err := free(""); err != nil || remaining < backupSpaceMinimumReserve {
		t.Fatal("cleanup did not preserve the modeled live reserve", remaining, err)
	}
	if _, err := db.AppendAuditAction(context.Background(), model.AuditActionSpec{Actor: "live", Action: "after-pressure", ResourceType: "fixture", ResourceID: "write"}); err != nil {
		t.Fatal("live database cannot commit after interrupted backup", err)
	}
}

func TestOnlineBackupSerializesMeasuredReserve(t *testing.T) {
	path, _ := backupFixture(t)
	_, cleanup, err := snapshotOnlineDatabase(t.Context(), path, DefaultBackupMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if _, _, err := snapshotOnlineDatabase(t.Context(), path, DefaultBackupMaxBytes); err == nil || !strings.Contains(err.Error(), "volume is busy") {
		t.Fatal("parallel backup can spend an active snapshot's reserve", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	_, cleanup, err = snapshotOnlineDatabase(t.Context(), path, DefaultBackupMaxBytes)
	if err != nil {
		t.Fatal("finished backup kept the volume locked", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestBackupSpaceGuardDoesNotCreditSparseFileSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse.sqlite")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const size int64 = 8 << 20
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	guard := backupSpaceGuard{volume: filepath.Dir(path), snapshot: path, size: size, reserve: backupSpaceMinimumReserve,
		free: func(string) (int64, error) { return backupSpaceMinimumReserve + backupSpaceMetadata, nil }}
	if err := guard.check(); !errors.Is(err, ErrBackupSpace) {
		t.Fatal("sparse size was treated as already allocated", err)
	}
}

func TestBackupSpaceStatfsInvalidAvailableCountFailsClosed(t *testing.T) {
	for _, available := range []uint64{^uint64(0), uint64(1) << 60} {
		free, err := backupVolumeFreeBytesWithStatfs("fixture", func(_ string, stat *unix.Statfs_t) error {
			stat.Bsize, stat.Bavail = 4096, available
			return nil
		})
		if err == nil || free != 0 {
			t.Fatal("invalid available count became a huge writable budget", available, free, err)
		}
	}
	free, err := backupVolumeFreeBytesWithStatfs("fixture", func(_ string, stat *unix.Statfs_t) error {
		stat.Bsize, stat.Bavail = 4096, 10
		return nil
	})
	if err != nil || free != 40960 {
		t.Fatal(free, err)
	}
}
