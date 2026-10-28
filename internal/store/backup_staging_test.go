package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestOnlineBackupStagedOnDatabaseVolume(t *testing.T) {
	path, _ := backupFixture(t)
	snapshot, cleanup, err := snapshotOnlineDatabase(t.Context(), path, DefaultBackupMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if filepath.Dir(filepath.Dir(snapshot)) != filepath.Dir(path) {
		t.Fatalf("snapshot must be on data volume, not /tmp: %s", snapshot)
	}
	info, err := os.Stat(filepath.Dir(snapshot))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
}

func TestOnlineBackupStaleStagingSweep(t *testing.T) {
	parent := t.TempDir()
	now := time.Now()
	old := now.Add(-onlineBackupStagingMaxAge - time.Minute)
	makeDir := func(name string) string {
		t.Helper()
		dir := filepath.Join(parent, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "oberth.sqlite"), []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	stale := makeDir(onlineBackupStagingPrefix + "interrupted")
	active := makeDir(onlineBackupStagingPrefix + "active")
	lock, err := lockOnlineBackupStaging(active)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	fresh := makeDir(onlineBackupStagingPrefix + "fresh")
	other := makeDir("unrelated")
	for _, dir := range []string{stale, active, other} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(parent, onlineBackupStagingPrefix+"symlink")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := sweepOnlineBackupStaging(parent, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("interrupted staging retained", err)
	}
	for _, dir := range []string{active, fresh, other, link} {
		if _, err := os.Lstat(dir); err != nil {
			t.Fatal("active, fresh or unrelated directory removed", dir, err)
		}
	}
	// Process exit releases this kernel lock. The next invocation can reclaim
	// the old directory without relying on a PID or guessing whether it is live.
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sweepOnlineBackupStaging(parent, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(active); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unlocked interrupted staging retained", err)
	}
}

func TestOnlineBackupCanceledBetweenStepsRemovesStaging(t *testing.T) {
	path, db := backupFixture(t)
	if _, err := db.db.Exec("CREATE TABLE pad(data BLOB); INSERT INTO pad VALUES(zeroblob(2097152))"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	steps := 0
	_, _, err := snapshotOnlineDatabaseSteps(ctx, path, DefaultBackupMaxBytes, func(backup *sqlite.Backup) (bool, error) {
		steps++
		more, err := backup.Step(128)
		if !more || err != nil {
			t.Fatal("fixture must need multiple steps", more, err)
		}
		cancel()
		return more, err
	})
	if !errors.Is(err, context.Canceled) || steps != 1 {
		t.Fatal(steps, err)
	}
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(path), onlineBackupStagingPrefix+"*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("canceled plaintext retained", entries, err)
	}
}

func TestOnlineBackupBadStaleEntryDoesNotBlockLaterCleanupOrBackup(t *testing.T) {
	path, _ := backupFixture(t)
	parent := filepath.Dir(path)
	bad := filepath.Join(parent, onlineBackupStagingPrefix+"aaa-bad")
	good := filepath.Join(parent, onlineBackupStagingPrefix+"zzz-good")
	for _, dir := range []string{bad, good} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(parent, "unrelated-lock-target")
	if err := os.WriteFile(target, []byte("do not touch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bad, ".active")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * onlineBackupStagingMaxAge)
	for _, dir := range []string{bad, good} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := sweepOnlineBackupStaging(parent, time.Now()); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatal("bad entry must be reported with its path", err)
	}
	if _, err := os.Stat(good); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("later stale entry was not attempted", err)
	}
	_, cleanup, err := snapshotOnlineDatabase(t.Context(), path, DefaultBackupMaxBytes)
	if err != nil {
		t.Fatal("one bad stale entry disabled backup", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "do not touch" {
		t.Fatal("sweep followed the marker symlink", err)
	}
}

func TestStartupBackupSweepRemovesFreshInterruptedButNotActive(t *testing.T) {
	parent := t.TempDir()
	fresh, cleanup, err := newOnlineBackupStaging(parent)
	if err != nil {
		t.Fatal(err)
	}
	// An active lock protects a snapshot independently of age.
	if err := SweepBackupStagingAtStartup(filepath.Join(parent, "oberth.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("startup removed a held snapshot", err)
	}
	// Model process death by dropping its lock without running its cleanup.
	// Open the marker separately only after releasing the original descriptor.
	// cleanup removes the active fixture; a fresh unowned directory models the
	// crash before lock creation, which startup must also reclaim immediately.
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	fresh = filepath.Join(parent, onlineBackupStagingPrefix+"just-interrupted")
	if err := os.Mkdir(fresh, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "oberth.sqlite"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SweepBackupStagingAtStartup(filepath.Join(parent, "oberth.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fresh crash staging survived restart", err)
	}
}
