package installer_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestBackupChangedStageIsNeitherPublishedNorRemoved(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"failure", "success"} {
		for _, kind := range []string{"file", "symlink", "directory"} {
			t.Run(outcome+"/"+kind, func(t *testing.T) { changedStageCase(t, outcome, kind) })
		}
	}
}

func changedStageCase(t *testing.T, outcome, kind string) {
	t.Helper()
	cfg, _ := backupConfig(t)
	source := cfg.Source
	var stage string
	moved := filepath.Join(filepath.Dir(cfg.Path), "moved-original")
	cfg.Source = snapshotFunc(func(ctx context.Context, o stateport.Options, out io.Writer) (stateport.BackupReport, error) {
		files, err := filepath.Glob(filepath.Join(filepath.Dir(cfg.Path), ".oberth-backup-incomplete-*"))
		if err != nil || len(files) != 1 {
			t.Fatal("staging file missing", err)
		}
		stage = files[0]
		if err = os.Rename(stage, moved); err != nil {
			t.Fatal(err)
		}
		occupyBackupPath(t, stage, kind)
		if outcome == "failure" {
			return stateport.BackupReport{}, errors.New("snapshot failed after stage moved")
		}
		return source.Snapshot(ctx, o, out)
	})
	if _, err := installer.Backup(t.Context(), cfg); err == nil {
		t.Fatal("changed staging path accepted")
	}
	assertOccupiedBackupPath(t, stage, kind)
	assertBackupUnpublished(t, cfg.Path)
	if _, err := os.Stat(moved); err != nil {
		t.Fatal("moved file was removed", err)
	}
}
