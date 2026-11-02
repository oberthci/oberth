package store

import (
	"testing"
	"time"
)

func TestV11RerunChecksForeignKeys(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	if _, err := s.db.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO repositories(name,upstream_id,default_branch,created_at,updated_at) VALUES('orphan',999999,'main',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := migrateV11CanonicalPersistence(t.Context(), s.db, time.Now); err == nil {
		t.Fatal("replayed migration ignored FK violation")
	}
}
