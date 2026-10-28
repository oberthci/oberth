package installer

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

func TestHistoryBackupLargeStreamAndRestoreRoundTrip(t *testing.T) {
	ctx := t.Context()
	base, _, runID := sourceDatabase(t)
	work := t.TempDir()
	basePath := filepath.Join(work, "base.tar")
	if err := os.WriteFile(basePath, base, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := extractDatabaseFiles(ctx, basePath, work, 0)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", files.Main)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE backup_padding(data BLOB); INSERT INTO backup_padding VALUES(zeroblob(178257920)); DROP TABLE backup_padding")
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	protocol := store.ProtocolConfig{SchemaIdentity: sourceCompat.SchemaIdentity, AuditDomain: sourceCompat.AuditDomain}
	databasePath := filepath.Join(work, "large.tar")
	f, err := os.OpenFile(databasePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.WriteVerifiedBackup(ctx, files.Main, store.LatestMigrationVersion(), store.Options{Protocol: protocol}, f)
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	info, err := os.Stat(databasePath)
	if err != nil || info.Size() <= 160<<20 {
		t.Fatal("fixture must exceed old outer member cap", info, err)
	}
	source := newFakeBackupCluster(t, sourceCompat)
	var output bytes.Buffer
	deps := source.deps(&output)
	stream := deps.runIO
	streamed := false
	deps.runStdout = func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("backup used buffered stdout")
		return nil, nil
	}
	deps.runIO = func(ctx context.Context, in io.Reader, out io.Writer, name string, args ...string) error {
		if strings.Contains(strings.Join(args, " "), "backup database") {
			if in != nil {
				t.Fatal("snapshot unexpectedly had stdin")
			}
			if !strings.Contains(strings.Join(args, " "), "--max-database-bytes 1073741824") {
				t.Fatal("snapshot limit not passed to pod", args)
			}
			file, err := os.Open(databasePath)
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			n, err := io.Copy(out, file)
			streamed = n == info.Size()
			return err
		}
		return stream(ctx, in, out, name, args...)
	}
	outer := filepath.Join(t.TempDir(), "history.cms")
	cfg := BackupConfig{Scope: BackupScopeHistory, Output: outer, Recipient: "fixture.pem"}
	cfg.applyDefaults()
	if err := runBackup(ctx, cfg, deps); err != nil {
		t.Fatal(err, output.String())
	}
	if !streamed {
		t.Fatal("large snapshot did not pass through streaming runner")
	}
	restored, err := loadRestoreArchive(ctx, RestoreConfig{Input: outer}, source.deps(&output), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if restored.Contents.Runs != 1 || restored.Database.Main == "" || restored.Database.WAL != "" {
		t.Fatal("restore lost history or retained a WAL", restored.Contents)
	}
	inspection, err := store.InspectCurrent(ctx, restored.LocalDatabase, store.Options{Protocol: protocol})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inspection.Close() }()
	if _, err := inspection.Run(ctx, runID); err != nil {
		t.Fatal("restored run missing", err)
	}
}
