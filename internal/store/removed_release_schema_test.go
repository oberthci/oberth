package store

import (
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestRemovedReleaseTablePreservesDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Create(t.Context(), path, testOptions())
	requireOK(t, err)
	requireOK(t, s.Close())
	db := pool(path, "rw", testOptions(), 1)
	execute(t, db, `CREATE TABLE release_roles(repo_id INTEGER PRIMARY KEY, account TEXT)`)
	requireOK(t, db.Close())
	_, err = Open(t.Context(), path, testOptions())
	requireCode(t, err, publicerr.SchemaRefused)
	refusal := publicerr.Classify(err)
	if refusal.Reason() != "Database layout is unsupported." || refusal.Remedy() != "Preserve this database; use its release or restore a verified backup." {
		t.Fatal("removed schema offered an unsupported upgrade", err)
	}
}
