package installer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/store"
)

// retainStoppedTargetGrants reads only fixed database files from the existing
// restore utility Pod, after every server Pod has stopped. A grant revoked just
// before shutdown must remain revoked after a history-only restore.
func retainStoppedTargetGrants(ctx context.Context, cfg RestoreConfig, deps backupDeps, archive *restoreArchive, target restoreTarget, pod string) error {
	limit, err := store.BackupMaxBytes(cfg.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(archive.LocalDatabase), "target-grants-*.tar")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	bounded := &grantSnapshotWriter{out: file, remaining: limit + 64<<10}
	if deps.runIO == nil {
		_ = file.Close()
		return errors.New("restore stream runner unavailable")
	}
	command := []string{"sh", "-c", `set -eu; cd /data; set -- oberth.sqlite; if [ -f oberth.sqlite-wal ]; then set -- "$@" oberth.sqlite-wal; fi; tar -cf - "$@"`}
	err = deps.runIO(ctx, nil, bounded, "kubectl", kubectlExecArgs(deps.ContextName, cfg.Namespace, pod, restoreHelperContainer, false, command...)...)
	if err = errors.Join(err, file.Close()); err != nil {
		return fmt.Errorf("read stopped target grant database: %w", err)
	}
	protocol := store.ProtocolConfig{SchemaIdentity: target.Server.Compatibility.SchemaIdentity, AuditDomain: target.Server.Compatibility.AuditDomain}
	_, grants, err := inspectGrantSnapshot(ctx, file.Name(), filepath.Dir(file.Name()), protocol, cfg.MaxDatabaseBytes)
	if err != nil {
		return fmt.Errorf("inspect stopped target grants: %w", err)
	}
	return preserveGrantsInRestore(ctx, cfg, archive, grants)
}

func preserveGrantsInRestore(ctx context.Context, cfg RestoreConfig, archive *restoreArchive, grants secretAccessExport) error {
	if err := validateExportedGrants(grants.Grants); err != nil {
		return err
	}
	compat := archive.Manifest.Compatibility
	opts := store.Options{Protocol: store.ProtocolConfig{SchemaIdentity: compat.SchemaIdentity, AuditDomain: compat.AuditDomain}, SnapshotTempDir: filepath.Dir(archive.LocalDatabase), BackupMaxBytes: cfg.MaxDatabaseBytes}
	db, err := store.OpenAdminClient(ctx, archive.LocalDatabase, opts)
	if err != nil {
		return err
	}
	err = db.ReplaceSecretAccess(ctx, "restore@operator", grants.Grants)
	if err = errors.Join(err, db.Close()); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(archive.LocalDatabase), "preserved-grants-*.tar")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	report, err := store.WriteVerifiedBackup(ctx, archive.LocalDatabase, archive.Manifest.Database.Schema, opts, file)
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	pair, err := extractDatabaseFiles(ctx, file.Name(), filepath.Dir(file.Name()), cfg.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	archive.Database, archive.LocalDatabase = pair, pair.Main
	archive.RestoredHeadID = report.AuditHead.ID
	archive.RestoredHeadSHA256 = hex.EncodeToString(report.AuditHead.SHA256)
	return nil
}

type grantSnapshotWriter struct {
	out       io.Writer
	remaining int64
}

func (w *grantSnapshotWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("stopped target database exceeds restore bound")
	}
	n, err := w.out.Write(p)
	w.remaining -= int64(n)
	return n, err
}
