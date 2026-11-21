package installer_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestBackupRefusalsNameTheRequestedDestination(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"concurrent destination", "missing parent", "source failure"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := backupConfig(t)
			code := "backup-destination"
			sourceError := errors.New("snapshot writer failed")
			switch kind {
			case "missing parent":
				cfg.Path = filepath.Join(filepath.Dir(cfg.Path), "missing", "backup.age")
			case "source failure":
				code = "backup"
				cfg.Source = snapshotFunc(func(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error) {
					return stateport.BackupReport{}, sourceError
				})
			default:
				code = "restore-exists"
				source := cfg.Source
				cfg.Source = snapshotFunc(func(ctx context.Context, o stateport.Options, out io.Writer) (stateport.BackupReport, error) {
					occupyBackupPath(t, cfg.Path, "file")
					return source.Snapshot(ctx, o, out)
				})
			}
			_, err := installer.Backup(t.Context(), cfg)
			var refusal *installer.Problem
			if !errors.As(err, &refusal) || refusal.Code != code || !strings.Contains(refusal.Detail, cfg.Path) {
				t.Fatal("refusal did not identify its destination and cause", err)
			}
			if kind == "source failure" && (!errors.Is(err, sourceError) || !strings.Contains(err.Error(), sourceError.Error())) {
				t.Fatal("source failure lost its displayed cause", err)
			}
		})
	}
}

func TestBackupFailuresKeepDisplayableCauses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cause error
		text  string
	}{
		{"certificate", errors.New("certificate expired"), "certificate expired"},
		{"filesystem", &os.PathError{Op: "write", Path: ".oberth-backup-incomplete-gone", Err: syscall.ENOSPC}, "no space left on device"},
		{"multiple filesystem causes", errors.Join(&os.PathError{Op: "write", Path: ".oberth-backup-incomplete-first", Err: syscall.ENOSPC}, &os.PathError{Op: "sync", Path: ".oberth-backup-incomplete-second", Err: syscall.EIO}), "input/output error"},
		{"joined", errors.Join(errors.New("certificate expired"), &os.PathError{Op: "write", Path: ".oberth-backup-incomplete-gone", Err: syscall.ENOSPC}), "certificate expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := backupConfig(t)
			cfg.Source = snapshotFunc(func(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error) {
				return stateport.BackupReport{}, tc.cause
			})
			_, err := installer.Backup(t.Context(), cfg)
			if !errors.Is(err, tc.cause) || !strings.Contains(err.Error(), cfg.Path) || !strings.Contains(err.Error(), tc.text) || strings.Contains(err.Error(), ".oberth-backup-incomplete-") {
				t.Fatal("backup lost path/cause or printed a vanished stage", err)
			}
			if tc.name == "joined" && !strings.Contains(err.Error(), "no space left on device") {
				t.Fatal("joined cause lost", err)
			}
		})
	}
}

func TestBackupStreamLimitIsDisplayed(t *testing.T) {
	t.Parallel()
	cfg, _ := backupConfig(t)
	cfg.Options.MaxBytes = 1
	var limitError error
	cfg.Source = snapshotFunc(func(_ context.Context, _ stateport.Options, out io.Writer) (stateport.BackupReport, error) {
		_, limitError = out.Write(make([]byte, (64<<10)+2))
		return stateport.BackupReport{}, limitError
	})
	_, err := installer.Backup(t.Context(), cfg)
	if err == nil || limitError == nil || !errors.Is(err, limitError) || !strings.Contains(err.Error(), cfg.Path) || !strings.Contains(err.Error(), "exceeds its selected bound") {
		t.Fatal("stream limit was hidden", err)
	}
}
