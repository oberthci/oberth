package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectDiscardedWorkspaceDatabase(t *testing.T) {
	for _, tc := range []struct{ name, sql, message string }{
		{"recorded_expiry_without_column", `ALTER TABLE token_credentials DROP COLUMN expires_at`, "token_credentials.expires_at"},
		{"comments_at_v38", integrationDowngradeSQL + `CREATE TABLE issue_comments(id INTEGER PRIMARY KEY)`, "issue_comments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "incompatible.sqlite")
			s, err := openAdminClientForTest(t.Context(), path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(t.Context(), tc.sql); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			for _, open := range []func() (*Store, error){
				func() (*Store, error) { return openAdminClientForTest(t.Context(), path, Options{}) },
				func() (*Store, error) { return OpenCurrent(t.Context(), path, Options{}) },
				func() (*Store, error) { return InspectCurrent(t.Context(), path, Options{}) },
			} {
				got, err := open()
				if got != nil {
					_ = got.Close()
				}
				if !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), "v38 backup") {
					t.Fatalf("discarded database must fail with recovery guidance, got %v", err)
				}
			}
		})
	}
}

// This fixture retains the priority tables and ledger entry at v39, while
// removing every later migration.
func TestUpgradePriorityV39Database(t *testing.T) {
	for _, opener := range []struct {
		name string
		open func(context.Context, string, Options) (*Store, error)
	}{{"owner", Open}, {"admin", OpenAdminClient}, {"current", OpenCurrent}} {
		t.Run(opener.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "priority-v39.sqlite")
			s, err := openAdminClientForTest(t.Context(), path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.db.ExecContext(t.Context(), restartRecoveryDowngradeSQL+publicationAbandonDowngradeSQL+`
DROP TRIGGER issues_metadata_revision;
ALTER TABLE issues DROP COLUMN revision;
ALTER TABLE token_credentials DROP COLUMN expires_at;
DELETE FROM schema_migrations WHERE version>=40;
INSERT INTO priority_grants(identity,max_class,expires,granted_by)
VALUES('operator', 'high', 999999999999999999, 'admin');`)
			if err != nil {
				t.Fatal(err)
			}
			var beforeCount, beforeVersion int
			if err := s.db.QueryRow(`SELECT count(*), max(version) FROM schema_migrations`).Scan(&beforeCount, &beforeVersion); err != nil || beforeCount != 39 || beforeVersion != 39 {
				t.Fatalf("historical ledger = %d rows through %d, want 39: %v", beforeCount, beforeVersion, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = opener.open(t.Context(), path, Options{})
			if err != nil {
				t.Fatalf("upgrade priority v39 database: %v", err)
			}
			defer s.Close()
			var version, grants int
			if err := s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if version != latestMigrationVersion {
				t.Fatalf("version=%d, want %d", version, latestMigrationVersion)
			}
			if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='priority_grants'`).Scan(&grants); err != nil || grants != 0 {
				t.Fatalf("retired priority table remains: %d %v", grants, err)
			}
			for _, query := range []string{`SELECT expires_at FROM token_credentials`, `SELECT revision FROM issues`} {
				rows, err := s.db.Query(query)
				if err != nil {
					t.Fatal(err)
				}
				rows.Close()
			}
		})
	}
}
