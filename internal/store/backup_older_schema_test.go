package store

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// A newer CLI backs up and summarizes an older server's database: oberth
// upgrade takes its pre-upgrade backup with the target release's CLI, before
// the server it backs up has migrated.
func TestNewerBinaryBacksUpAndSummarizesOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "older.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, now: time.Now, protocol: DefaultProtocolConfig()}
	older := migrations[:len(migrations)-1]
	if err := s.migrateWithMigrations(t.Context(), nil, older); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	schema := latestMigrationVersion - 1
	var archive bytes.Buffer
	report, err := WriteVerifiedBackup(t.Context(), path, schema, Options{}, &archive)
	if err != nil {
		t.Fatalf("back up schema %d with a schema %d binary: %v", schema, latestMigrationVersion, err)
	}
	if report.Schema != schema {
		t.Fatalf("report schema %d", report.Schema)
	}
	if _, err := VerifyBackupArchive(t.Context(), bytes.NewReader(archive.Bytes()), schema, Options{SnapshotTempDir: t.TempDir()}); err != nil {
		t.Fatalf("verify an older-schema archive: %v", err)
	}
	inspection, err := InspectCurrent(t.Context(), path, Options{SnapshotTempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("inspect an older schema: %v", err)
	}
	defer func() { _ = inspection.Close() }()
	if _, err := inspection.Summary(t.Context()); err != nil {
		t.Fatalf("summarize an older schema: %v", err)
	}
}
