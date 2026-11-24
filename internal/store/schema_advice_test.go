package store

import (
	"bytes"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestMigrationGapRefusalDoesNotOpenWriter(t *testing.T) {
	path, _ := migrationFixturePath(t, 6)
	db := pool(path, "rw", testOptions(), 1)
	var mode string
	requireOK(t, db.QueryRowContext(t.Context(), `PRAGMA journal_mode=DELETE`).Scan(&mode))
	requireOK(t, db.Close())
	if mode != "delete" {
		t.Fatal("fixture is not in rollback journal mode", mode)
	}
	before, err := os.ReadFile(path)
	requireOK(t, err)
	target := testMigrationTarget()
	delete(target.migrations, 7)
	_, err = open(t.Context(), path, testOptions(), target)
	requireCode(t, err, publicerr.SchemaRefused)
	problem := publicerr.Classify(err)
	if problem.Reason() != "No upgrade: schema 6." || problem.Remedy() != "Use a release that upgrades schema 6, or restore a verified backup." {
		t.Fatal("gap refusal did not name the unchanged source schema", err)
	}
	after, err := os.ReadFile(path)
	requireOK(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("gap refusal rewrote the database")
	}
}

func TestOlderBackupNamesItsSchemaReader(t *testing.T) {
	s := testStore(t)
	_, _ = accepted(t, s, "backup-reader")
	var archive bytes.Buffer
	_, err := s.Snapshot(t.Context(), backupOptions(s), &archive)
	requireOK(t, err)
	options := backupOptions(s)
	options.Schema++
	_, err = backupForTest(t, s).Inspect(t.Context(), options, bytes.NewReader(archive.Bytes()))
	requireCode(t, err, publicerr.SchemaRefused)
	problem := publicerr.Classify(err)
	if problem.Reason() != "Cannot read schema 6." || problem.Remedy() != "Use a release that reads schema 6; restore backups there, then upgrade." {
		t.Fatal("older backup did not name a compatible release", err)
	}
	version, _, err := s.StateMetadata(t.Context())
	requireOK(t, err)
	if version != 6 {
		t.Fatal("inspection changed the database schema", version)
	}
}

func TestSchemaRecoveryAdviceFitsMessageBounds(t *testing.T) {
	for _, kind := range []refusal{schema, newerSchema} {
		_, reason, remedy := message(kind)
		requireOK(t, publicerr.CheckMessage(reason, 40))
		requireOK(t, publicerr.CheckMessage(remedy, 120))
	}
	max := int(^uint(0) >> 1)
	for _, kind := range []refusal{migrationGap, olderSchema} {
		for _, number := range []int{5, 6, max, -max - 1} {
			err := publicerr.Classify(schemaVersionRefusal(kind, number))
			requireOK(t, publicerr.CheckMessage(err.Reason(), 40))
			requireOK(t, publicerr.CheckMessage(err.Remedy(), 120))
		}
	}
}
