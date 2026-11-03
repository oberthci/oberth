package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectDiscardedWorkspaceDatabase(t *testing.T) {
	for _, tc := range []struct{ name, sql, message string }{
		{"v39_without_expiry", `ALTER TABLE token_credentials DROP COLUMN expires_at`, "token_credentials.expires_at"},
		{"comments_at_v38", integrationDowngradeSQL + `CREATE TABLE issue_comments(id INTEGER PRIMARY KEY)`, "issue_comments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "incompatible.sqlite")
			s, err := OpenAdminClient(t.Context(), path, Options{})
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
				func() (*Store, error) { return OpenAdminClient(t.Context(), path, Options{}) },
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
