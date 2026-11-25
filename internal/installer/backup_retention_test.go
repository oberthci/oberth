package installer_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestDirectoryBackupsKeepNewestOnlyAfterSuccess(t *testing.T) {
	t.Parallel()
	source, options := backupFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "operator.age"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	c := installer.FullArchiveForTest(installer.BackupConfig{Source: source, Options: options, Recipient: identity.Recipient().String(), Directory: dir, Keep: 2})
	for i := range 4 {
		c.Now = time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC)
		archive, err := installer.Backup(t.Context(), c)
		if err != nil || archive.SHA256 == "" {
			t.Fatal("directory backup failed", err)
		}
	}
	names := retentionNames(t, dir)
	if len(names) != 2 || !strings.Contains(names[0], "20261103") || !strings.Contains(names[1], "20261104") {
		t.Fatal("retention did not keep the newest two backups", names)
	}
	checkRetentionFailure(t, c, names)
}

func checkRetentionFailure(t *testing.T, c installer.BackupConfig, names []string) {
	t.Helper()
	source, dir := c.Source, c.Directory
	c.Now = c.Now.AddDate(0, 0, 1)
	c.Keep, c.Source = 1, brokenSnapshot{}
	if _, err := installer.Backup(t.Context(), c); err == nil || strings.Join(retentionNames(t, dir), ",") != strings.Join(names, ",") {
		t.Fatal("failed backup pruned a good archive", err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "operator.age")); err != nil || string(raw) != "keep" {
		t.Fatal("retention touched an unrelated archive", err)
	}
	c.Source, c.Now = source, c.Now.AddDate(0, 0, -20)
	archive, err := installer.Backup(t.Context(), c)
	if err != nil || len(retentionNames(t, dir)) != 1 {
		t.Fatal("clock reversal broke the retention bound", err)
	}
	if _, err = os.Stat(archive.Path); err != nil {
		t.Fatal("retention removed the just-completed backup", err)
	}
}

func TestDirectoryBackupRetentionRemovesExpiredOriginManifests(t *testing.T) {
	t.Parallel()
	source, options := backupFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := installer.FullArchiveForTest(installer.BackupConfig{Source: source, Options: options, Recipient: identity.Recipient().String(), Directory: dir, Keep: 1})
	var paths []string
	for i := range 2 {
		c.Now = time.Date(2026, 11, 1+i, 0, 0, 0, 0, time.UTC)
		archive, err := installer.Backup(t.Context(), c)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, archive.Path)
	}
	c.Now = c.Now.AddDate(0, 0, 1)
	if _, err = installer.Backup(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err = os.Lstat(path + ".manifest.json"); !os.IsNotExist(err) {
			t.Fatal("retention left an expired origin manifest", filepath.Base(path), err)
		}
	}
}

// replaceWithNonregular moves the archive aside and leaves a link or directory at its name.
func replaceWithNonregular(t *testing.T, path, kind string) string {
	t.Helper()
	target := filepath.Join(filepath.Dir(path), "operator-copy.age")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	var err error
	if kind == "symlink" {
		err = os.Symlink(target, path)
	} else {
		err = os.Mkdir(path, 0700)
	}
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func retentionNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "oberth-backup-") && strings.HasSuffix(entry.Name(), ".age") {
			names = append(names, entry.Name())
		}
	}
	return names
}

type brokenSnapshot struct{}

func (brokenSnapshot) Snapshot(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error) {
	return stateport.BackupReport{}, errors.New("snapshot failed")
}

func TestDirectoryBackupDryRunAndKeepBounds(t *testing.T) {
	t.Parallel()
	source, options := backupFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []int{0, 1, 1000, 1001} {
		dir := t.TempDir()
		archive, err := installer.Backup(t.Context(), installer.BackupConfig{Source: source, Options: options, Recipient: identity.Recipient().String(), Directory: dir, Keep: keep, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), DryRun: true})
		if (err == nil) != (keep >= 1 && keep <= 1000) || len(retentionNames(t, dir)) != 0 || (err == nil && !archive.DryRun) {
			t.Fatal("retention bound or dry run failed", keep, err)
		}
	}
}

func TestDirectoryBackupRetentionPreservesMatchingLinksAndDirectories(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			source, options := backupFixture(t)
			identity, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			c := installer.FullArchiveForTest(installer.BackupConfig{Source: source, Options: options, Recipient: identity.Recipient().String(),
				Directory: t.TempDir(), Keep: 1, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)})
			old, err := installer.Backup(t.Context(), c)
			if err != nil {
				t.Fatal(err)
			}
			target := replaceWithNonregular(t, old.Path, kind)
			c.Now = c.Now.AddDate(0, 0, 1)
			if _, err = installer.Backup(t.Context(), c); err != nil {
				t.Fatal("retention touched a nonregular backup name", kind, err)
			}
			info, err := os.Lstat(old.Path)
			if err != nil || (kind == "symlink" && info.Mode()&os.ModeSymlink == 0) || (kind == "directory" && !info.IsDir()) {
				t.Fatal("retention removed a nonregular backup name", kind, err)
			}
			if _, err = os.Stat(target); err != nil {
				t.Fatal("retention changed the operator's copy", err)
			}
		})
	}
}

// A database-only archive cannot be restored, so it must not trigger pruning of a full one.
func TestDirectoryBackupRefusesDatabaseOnlyArchive(t *testing.T) {
	t.Parallel()
	source, options := backupFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	full := []string{"oberth-backup-20261001T000000.000000000Z.age", "oberth-backup-20261001T000000.000000000Z.age.manifest.json"}
	writeRetained(t, dir, full...)
	c := installer.BackupConfig{Source: source, Options: options, Recipient: identity.Recipient().String(), Directory: dir, Keep: 1, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	var refusal *publicerr.Error
	if _, err = installer.Backup(t.Context(), c); !errors.As(err, &refusal) || refusal.Reason() != "Retention needs a full backup." {
		t.Fatal("database-only directory backup was not refused", err)
	}
	if got := strings.Join(dirNames(t, dir), ","); got != strings.Join(full, ",") {
		t.Fatal("refused database-only backup wrote or pruned an archive", got)
	}
}

// Retention counts and prunes only signed full archives; database-only copies are left alone.
func TestDirectoryRetentionCountsOnlyFullArchives(t *testing.T) {
	t.Parallel()
	source, options := backupFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	full := []string{"oberth-backup-20261001T000000.000000000Z.age", "oberth-backup-20261001T000000.000000000Z.age.manifest.json"}
	partial := []string{"oberth-backup-20261002T000000.000000000Z.age", "oberth-backup-20261003T000000.000000000Z.age"}
	writeRetained(t, dir, append(full, partial...)...)
	c := installer.FullArchiveForTest(installer.BackupConfig{Source: source, Options: options, Recipient: identity.Recipient().String(), Directory: dir, Keep: 2, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)})
	first, err := installer.Backup(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(slices.Clone(full), partial...), filepath.Base(first.Path), filepath.Base(first.Path)+".manifest.json")
	slices.Sort(want)
	if got := dirNames(t, dir); !slices.Equal(got, want) {
		t.Fatal("database-only archives counted toward retention", got)
	}
	c.Keep, c.Now = 1, c.Now.AddDate(0, 0, 1)
	second, err := installer.Backup(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	want = append(slices.Clone(partial), filepath.Base(second.Path), filepath.Base(second.Path)+".manifest.json")
	slices.Sort(want)
	if got := dirNames(t, dir); !slices.Equal(got, want) {
		t.Fatal("retention kept an expired full archive or pruned a database-only one", got)
	}
}

func writeRetained(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("prior-archive"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
