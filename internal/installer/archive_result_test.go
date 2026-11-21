package installer_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestBackupResultIncludesStoreSnapshotHint(t *testing.T) {
	t.Parallel()
	cfg, _ := backupConfig(t)
	archive, err := installer.Backup(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	result := archiveResultJSON(t, archive)
	if _, planned := result["dry_run"]; planned {
		t.Fatal("completed backup marked as a dry run")
	}
	if result["store_snapshot_hint"] != installer.StoreSnapshotHint {
		t.Fatal("successful backup omitted separate store-snapshot guidance")
	}
}

func archiveResultJSON(t *testing.T, archive installer.Archive) map[string]any {
	t.Helper()
	raw, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestBackupDryRunReturnsOnlyPlannedDestination(t *testing.T) {
	t.Parallel()
	cfg, _ := backupConfig(t)
	cfg.DryRun = true
	cfg.Source = snapshotFunc(func(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error) {
		t.Fatal("dry run opened snapshot")
		return stateport.BackupReport{}, nil
	})
	archive, err := installer.Backup(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	result := archiveResultJSON(t, archive)
	if result["dry_run"] != true || result["path"] != cfg.Path {
		t.Error("dry run omitted planned destination or mode")
	}
	for _, field := range []string{"sha256", "report"} {
		if _, ok := result[field]; ok {
			t.Errorf("dry run presented unmeasured %s as a completed backup", field)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.Path))
	if err != nil || len(entries) != 0 {
		t.Fatal("dry run created files", err)
	}
}

func TestBackupDryRunRefusesInvalidRequestsWithoutSnapshot(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"recipient", "options", "existing destination", "missing parent", "file parent", "empty path", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := backupConfig(t)
			cfg.DryRun = true
			cfg.Source = snapshotFunc(func(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error) {
				t.Fatal("dry run opened snapshot")
				return stateport.BackupReport{}, nil
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "recipient":
				cfg.Recipient = "invalid"
			case "options":
				cfg.Options.Schema = -1
			case "existing destination":
				occupyBackupPath(t, cfg.Path, "file")
			case "empty path":
				cfg.Path = ""
			case "missing parent":
				cfg.Path = filepath.Join(filepath.Dir(cfg.Path), "missing", "backup.age")
			case "file parent":
				occupyBackupPath(t, cfg.Path, "file")
				cfg.Path = filepath.Join(cfg.Path, "backup.age")
			case "cancelled":
				cancel()
			}
			if _, err := installer.Backup(ctx, cfg); err == nil {
				t.Fatal("dry run accepted invalid request")
			}
			switch kind {
			case "file parent":
				assertOccupiedBackupPath(t, filepath.Dir(cfg.Path), "file")
			case "existing destination":
				assertOccupiedBackupPath(t, cfg.Path, "file")
			default:
				assertBackupUnpublished(t, cfg.Path)
			}
		})
	}
}
