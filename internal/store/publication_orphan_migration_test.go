package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestV43MigrationRejectsOrphanPublicationAtV38(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orphan-v38.sqlite")
	s, err := OpenAdminClient(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), integrationDowngradeSQL); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Legacy data can predate foreign-key enforcement. Preserve it on refusal.
	if _, err := raw.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF;
 INSERT INTO publications(id,repo_id,run_id,ref_kind,ref,result_sha,actor,created_at,updated_at)
 VALUES('orphan-publication',999,'missing-run','branch','main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','fixture',1,1);`); err != nil {
		t.Fatal(err)
	}
	got, err := OpenAdminClient(t.Context(), path, Options{})
	if got != nil {
		_ = got.Close()
	}
	if !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), "migration 43") || !strings.Contains(err.Error(), "PRAGMA foreign_key_check") || !strings.Contains(err.Error(), "publications") {
		t.Fatalf("orphan upgrade must fail with actionable integrity diagnosis: %v", err)
	}
	var version, count int
	if err := raw.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM publications WHERE id='orphan-publication'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if version != 38 || count != 1 {
		t.Fatalf("failed migration changed fixture: version=%d orphan count=%d", version, count)
	}
}
