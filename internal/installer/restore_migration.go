package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/store"
)

// preflightRestoreMigration rehearses, on a scratch copy and before the
// swap, the migration the target server runs on its first start when the
// backup's schema is older than the server's. A migration that fails there
// would crash-loop the server on the restored database. The rehearsal uses
// this CLI's migrations, so it runs only when the CLI's schema is the
// target's; otherwise it says that the first start migrates unrehearsed and
// where the previous database stays.
func preflightRestoreMigration(ctx context.Context, archive restoreArchive, targetSchema int, previous string, w io.Writer) error {
	from := archive.Manifest.Database.Schema
	if targetSchema <= from {
		return nil
	}
	if targetSchema != store.LatestMigrationVersion() {
		_, _ = fmt.Fprintf(w, "  ! the server migrates the restored database from schema %d to %d on its first start; this CLI (schema %d) cannot rehearse that migration. "+
			"If the server does not start, the previous database is in %s\n", from, targetSchema, store.LatestMigrationVersion(), previous)
		return nil
	}
	// Beside the extracted database: the restore's private working directory.
	dir, err := os.MkdirTemp(filepath.Dir(archive.Database.Main), "migration-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	scratch := filepath.Join(dir, "oberth.sqlite")
	if err := copyPrivateFile(archive.Database.Main, scratch); err != nil {
		return err
	}
	if archive.Database.WAL != "" {
		if err := copyPrivateFile(archive.Database.WAL, scratch+"-wal"); err != nil {
			return err
		}
	}
	compat := archive.Manifest.Compatibility
	opts := store.Options{Protocol: store.ProtocolConfig{SchemaIdentity: compat.SchemaIdentity, AuditDomain: compat.AuditDomain}}
	migrated, err := store.OpenAdminClient(ctx, scratch, opts)
	if err != nil {
		return fmt.Errorf("the backup's database (schema %d) does not migrate to the target's schema %d (rehearsed on a copy; nothing was changed): %w", from, targetSchema, err)
	}
	if err := migrated.Close(); err != nil {
		return fmt.Errorf("close the rehearsed migration: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  ✓ migration from schema %d to %d rehearsed on a copy of the restored database\n", from, targetSchema)
	return nil
}

func copyPrivateFile(from, to string) (resultErr error) {
	source, err := os.Open(from) //nolint:gosec // G304: verified member in the private restore working directory.
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: fresh private directory.
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, target.Close()) }()
	_, err = io.Copy(target, source)
	return err
}
