package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// The small v0.17.83 release owns this index at v40, before bearer expiry.
// Keep the fixture SQL independent of the candidate's migration definitions.
const releaseV83IndexFixture = `DROP INDEX IF EXISTS runs_latest_ref_idx;
CREATE INDEX runs_latest_ref_idx ON runs(repo_id, ref_kind, ref, queue_sequence DESC, status, started_at, finished_at, queued_at, id);`

func releaseSplitLedger(t *testing.T, db *sql.DB) map[int]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT version, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ledger := map[int]int64{}
	for rows.Next() {
		var version int
		var applied int64
		if err := rows.Scan(&version, &applied); err != nil {
			t.Fatal(err)
		}
		ledger[version] = applied
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ledger
}

func TestReleaseSplitUpgradeFromV39AndV40(t *testing.T) {
	for _, version := range []int{39, 40} {
		for name, open := range map[string]func(context.Context, string, Options) (*Store, error){"admin": OpenAdminClient, "current": OpenCurrent} {
			t.Run(fmt.Sprintf("v%d/%s", version, name), func(t *testing.T) {
				now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
				path := filepath.Join(t.TempDir(), "release.sqlite")
				s, err := openAdminClientForTest(t.Context(), path, Options{Now: func() time.Time { return now }})
				if err != nil {
					t.Fatal(err)
				}
				repo := createRepo(t, s)
				run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", strings.Repeat("a", 40)))
				if err != nil {
					t.Fatal(err)
				}
				issue, err := s.CreateManualIssue(t.Context(), "fixture", model.ManualIssueSpec{Title: "preserved", Body: "history"})
				if err != nil {
					t.Fatal(err)
				}
				_, digest := lifecycleUplink(t, s, "legacy", "SHA256:release-split", false)
				if _, err = s.db.Exec(integrationDowngradeSQL + prioritySubmissionSchema + `
INSERT INTO schema_migrations(version,applied_at) VALUES(39,3900);
INSERT INTO priority_grants(identity,max_class,expires,granted_by) VALUES('operator','high',999999999999999999,'admin');`); err != nil {
					t.Fatal(err)
				}
				if version == 40 {
					if _, err = s.db.Exec(releaseV83IndexFixture + `INSERT INTO schema_migrations(version,applied_at) VALUES(40,4000);`); err != nil {
						t.Fatal(err)
					}
				}
				oldLedger := releaseSplitLedger(t, s.db)
				if len(oldLedger) != version {
					t.Fatalf("fixture ledger=%v", oldLedger)
				}
				var expiry int
				if err = s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('token_credentials') WHERE name='expires_at'`).Scan(&expiry); err != nil || expiry != 0 {
					t.Fatalf("fixture expiry=%d err=%v", expiry, err)
				}
				beforeAudit, err := s.VerifyAuditChain(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				inspection, err := InspectCurrent(t.Context(), path, Options{})
				if err != nil {
					t.Fatalf("inspect v%d: %v", version, err)
				}
				if !reflect.DeepEqual(oldLedger, releaseSplitLedger(t, inspection.db)) {
					t.Fatal("inspection changed the historical ledger")
				}
				if err := inspection.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = open(t.Context(), path, Options{Now: func() time.Time { return now.Add(time.Hour) }})
				if err != nil {
					t.Fatalf("upgrade v%d: %v", version, err)
				}
				defer s.Close()
				ledger := releaseSplitLedger(t, s.db)
				if len(ledger) != latestMigrationVersion {
					t.Fatalf("ledger has %d migrations, want %d", len(ledger), latestMigrationVersion)
				}
				for v := 1; v <= latestMigrationVersion; v++ {
					if _, ok := ledger[v]; !ok {
						t.Fatalf("missing migration %d", v)
					}
					if at, ok := oldLedger[v]; ok && ledger[v] != at {
						t.Fatalf("migration %d replayed: %d -> %d", v, at, ledger[v])
					}
				}
				assertSchemaContains(t, s.db, "index", "runs_latest_ref_idx", "status", "started_at", "finished_at", "queued_at", "id")
				gotRun, err := s.Run(t.Context(), run.ID)
				if err != nil || gotRun.SHA != run.SHA || gotRun.QueueSequence != run.QueueSequence || gotRun.Status != run.Status {
					t.Fatalf("run changed: %+v %v", gotRun, err)
				}
				gotIssue, err := s.Issue(t.Context(), issue.ID)
				if err != nil || gotIssue.Body != issue.Body || gotIssue.Title != issue.Title || gotIssue.WorkStatus != model.IssueTodo {
					t.Fatalf("issue changed: %+v %v", gotIssue, err)
				}
				token, err := s.TokenCredentialByDigest(t.Context(), digest)
				if err != nil || token.ExpiresAt != nil {
					t.Fatalf("token changed: %+v %v", token, err)
				}
				var grants int
				if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='priority_grants'`).Scan(&grants); err != nil || grants != 0 {
					t.Fatalf("retired priority table remains: %d %v", grants, err)
				}
				afterAudit, err := s.VerifyAuditChain(t.Context())
				if err != nil || !reflect.DeepEqual(beforeAudit, afterAudit) {
					t.Fatalf("audit changed: %v", err)
				}
				rows, err := s.db.Query(`PRAGMA foreign_key_check`)
				if err != nil {
					t.Fatal(err)
				}
				if rows.Next() {
					t.Error("foreign key violation after upgrade")
				}
				if err = rows.Err(); err != nil {
					t.Error(err)
				}
				rows.Close()
				if err = s.migrate(t.Context(), nil); err != nil {
					t.Fatal(err)
				}
				if again := releaseSplitLedger(t, s.db); !reflect.DeepEqual(ledger, again) {
					t.Fatal("second migration changed the ledger")
				}
			})
		}
	}
}

func TestReleaseSplitRejectsInvalidV40(t *testing.T) {
	for name, schema := range map[string]string{
		"old candidate expiry":   tokenExpirySchema,
		"missing covering index": "DROP INDEX runs_latest_ref_idx;",
		"wrong index order":      strings.Replace(releaseV83IndexFixture, "queue_sequence DESC", "queue_sequence ASC", 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid-v40.sqlite")
			s, err := openAdminClientForTest(t.Context(), path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec(integrationDowngradeSQL + prioritySubmissionSchema + schema + `
INSERT INTO schema_migrations(version,applied_at) VALUES(39,3900),(40,4000);`); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			for _, open := range []func(context.Context, string, Options) (*Store, error){OpenAdminClient, OpenCurrent, InspectCurrent} {
				before := readAuthoritativeDatabaseFiles(t, path)
				got, err := open(t.Context(), path, Options{})
				if got != nil {
					got.Close()
				}
				if !errors.Is(err, ErrSchemaIncompatible) || !strings.Contains(err.Error(), "v40") || !strings.Contains(err.Error(), "backup") {
					t.Errorf("invalid v40 should be refused with recovery guidance: %v", err)
				}
				assertDatabaseFilesEqual(t, readAuthoritativeDatabaseFiles(t, path), before, "invalid v40 refusal")
			}
		})
	}
}
