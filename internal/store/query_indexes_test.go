package store

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func schema6Fixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/schema6.sqlite.gz")
	requireOK(t, err)
	z, err := gzip.NewReader(bytes.NewReader(raw))
	requireOK(t, err)
	data, err := io.ReadAll(z)
	requireOK(t, err)
	requireOK(t, z.Close())
	path := filepath.Join(t.TempDir(), "state.sqlite")
	requireOK(t, os.WriteFile(path, data, 0600))
	return path
}

func TestSchema6OpenAddsOnlyQueryIndexes(t *testing.T) {
	t.Parallel()
	path := schema6Fixture(t)
	db := pool(path, "ro", testOptions(), 1)
	var indexes int
	requireOK(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE name IN ('issue_totals','issue_ci')`).Scan(&indexes))
	if indexes != 0 {
		t.Fatal("fixture already has query indexes")
	}
	head := independentHead(t, db)
	requireOK(t, db.Close())
	// An archive made before the indexes is valid without modifying it.
	options := backupOptions(&Store{options: testOptions()})
	report, err := verifyDatabase(t.Context(), path, options)
	requireOK(t, err)
	if report.AuditHead != head || report.Schema != 6 {
		t.Fatal("schema-six archive lost its identity", report)
	}
	for range 2 {
		s, err := Open(t.Context(), path, testOptions())
		requireOK(t, err)
		assertSchema6Records(t, s)
		if independentHead(t, s.readers) != head {
			t.Fatal("adding query indexes changed the audit")
		}
		s.assertIssueIndex(t, ciIssueLookup, []any{"origin/team/repo", "refs/heads/main"}, "issue_ci")
		requireOK(t, s.readers.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE name IN ('issue_totals','issue_ci')`).Scan(&indexes))
		if indexes != 2 {
			t.Fatal("open did not add both indexes", indexes)
		}
		requireOK(t, s.Close())
	}
	report, err = verifyDatabase(t.Context(), path, options)
	requireOK(t, err)
	if report.AuditHead != head || report.Schema != 6 {
		t.Fatal("indexed archive changed identity", report)
	}
}

func assertSchema6Records(t *testing.T, s *Store) {
	t.Helper()
	schema, protocol, err := s.StateMetadata(t.Context())
	requireOK(t, err)
	if schema != 6 || protocol != testOptions().Protocol {
		t.Fatal("query indexes changed the protocol", schema, protocol)
	}
	r, err := s.Run(t.Context(), "schema-six-run")
	requireOK(t, err)
	if r.Repo != "origin/team/repo" || !r.HasAdmission || r.Admission.Digest == "" {
		t.Fatal("schema-six run or admission lost")
	}
	for _, id := range []int64{50, 90} {
		issue, err := s.Issue(t.Context(), id)
		requireOK(t, err)
		if issue.Title != "Schema six issue" || issue.Body != "Recorded before optional query indexes" {
			t.Fatal("schema-six issue changed", id)
		}
	}
}

func TestQueryIndexDefinitionStillVerified(t *testing.T) {
	for _, statement := range []string{
		`CREATE INDEX issue_totals ON issues(state)`,
		`CREATE INDEX issue_ci ON issues(repo)`,
		`CREATE TABLE issue_ci(value TEXT)`,
	} {
		t.Run(statement, func(t *testing.T) {
			path := schema6Fixture(t)
			db := pool(path, "rw", testOptions(), 1)
			_, err := db.ExecContext(t.Context(), statement)
			requireOK(t, err)
			requireOK(t, db.Close())
			_, err = Open(t.Context(), path, testOptions())
			requireCode(t, err, publicerr.SchemaRefused)
			_, err = verifyDatabase(t.Context(), path, backupOptions(&Store{options: testOptions()}))
			requireCode(t, err, publicerr.SchemaRefused)
		})
	}
}
