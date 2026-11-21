package installer_test

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestBackupReportMatchesRequestedSchema(t *testing.T) {
	t.Parallel()
	cfg, _ := backupConfig(t)
	source := cfg.Source
	cfg.Source = snapshotFunc(func(ctx context.Context, options stateport.Options, out io.Writer) (stateport.BackupReport, error) {
		report, err := source.Snapshot(ctx, options, out)
		report.Schema++
		return report, err
	})
	_, err := installer.Backup(t.Context(), cfg)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "restore-proof" {
		t.Fatal("mismatched snapshot report was published", err)
	}
	assertBackupUnpublished(t, cfg.Path)
}

func TestBackupStreamIncludesExactLimit(t *testing.T) {
	t.Parallel()
	for _, size := range []int{(64 << 10) + 1, (64 << 10) + 2} {
		t.Run(map[bool]string{true: "at limit", false: "over limit"}[size == (64<<10)+1], func(t *testing.T) {
			cfg, _ := backupConfig(t)
			report, err := cfg.Source.Snapshot(t.Context(), cfg.Options, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Options.MaxBytes = 1
			var writeErr error
			cfg.Source = snapshotFunc(func(_ context.Context, _ stateport.Options, out io.Writer) (stateport.BackupReport, error) {
				_, writeErr = out.Write(make([]byte, size))
				return report, writeErr
			})
			_, err = installer.Backup(t.Context(), cfg)
			if size == (64<<10)+1 {
				if err != nil {
					t.Fatal("exactly bounded snapshot refused", err)
				}
			} else {
				var refusal *installer.Problem
				if !errors.As(writeErr, &refusal) || refusal.Code != "backup-limit" || !errors.Is(err, writeErr) {
					t.Fatal("oversized snapshot was not refused by stream bound", err)
				}
				assertBackupUnpublished(t, cfg.Path)
			}
		})
	}
}

func TestBackupWriterStopsOnCancellation(t *testing.T) {
	t.Parallel()
	cfg, _ := backupConfig(t)
	report, err := cfg.Source.Snapshot(t.Context(), cfg.Options, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var written int
	var writeErr error
	cfg.Source = snapshotFunc(func(_ context.Context, _ stateport.Options, out io.Writer) (stateport.BackupReport, error) {
		cancel()
		written, writeErr = out.Write([]byte("snapshot after cancellation"))
		return report, writeErr
	})
	_, err = installer.Backup(ctx, cfg)
	if written != 0 || !errors.Is(writeErr, context.Canceled) || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled source write was accepted", written, writeErr, err)
	}
	if _, err := os.Lstat(cfg.Path); !os.IsNotExist(err) {
		t.Fatal("canceled archive was published", err)
	}
}
