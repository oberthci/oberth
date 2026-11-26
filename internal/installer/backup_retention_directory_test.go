package installer_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestDirectoryBackupPreservesOlderCopiesWhenCurrentManifestIsLost(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) { checkDirectoryManifestLoss(t, mode) })
	}
}

func checkDirectoryManifestLoss(t *testing.T, mode string) {
	t.Helper()
	cfg, _ := backupConfig(t)
	cfg.Path, cfg.Directory = "", filepath.Join(t.TempDir(), "backups")
	if err := os.Mkdir(cfg.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Keep, cfg.Now = 1, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cfg = installer.FullArchiveForTest(cfg)
	older, err := installer.Backup(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Now = cfg.Now.AddDate(0, 0, 1)
	current := "oberth-backup-20261102T000000.000000000Z.age"
	original, moved := cfg.Source, cfg.Directory+"-pinned"
	cfg.Source = snapshotFunc(func(ctx context.Context, options stateport.Options, out io.Writer) (stateport.BackupReport, error) {
		// Keep publication's staging inode intact while the pinned retention
		// directory loses the new manifest. No timing or production hook is needed.
		replaceRetentionDirectory(t, cfg.Directory, moved)
		if mode == "corrupt" {
			if err := os.WriteFile(filepath.Join(moved, current+".manifest.json"), []byte("damaged-manifest"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return original.Snapshot(ctx, options, out)
	})
	archive, err := installer.Backup(t.Context(), cfg)
	if err == nil || archive.SHA256 == "" {
		t.Fatal("completed backup ignored loss of its manifest before pruning", err)
	}
	for _, path := range []string{archive.Path, archive.Path + ".manifest.json", filepath.Join(moved, filepath.Base(older.Path)), filepath.Join(moved, filepath.Base(older.Path)+".manifest.json")} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal("current-manifest refusal lost a completed archive or older recovery data", err)
		}
	}
}

func replaceRetentionDirectory(t *testing.T, dir, moved string) {
	t.Helper()
	stages, err := filepath.Glob(filepath.Join(dir, ".oberth-backup-incomplete-*"))
	if err != nil || len(stages) != 1 {
		t.Fatal("expected one open staging file", err)
	}
	if err = os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(moved, filepath.Base(stages[0])), stages[0]); err != nil {
		t.Fatal(err)
	}
}
