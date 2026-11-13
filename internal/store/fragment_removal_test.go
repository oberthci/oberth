package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestFragmentRunIndexRemovalPreservesRuns(t *testing.T) {
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, migration := range migrations {
		if strings.Contains(migration.id, "-remove-fragment-run-index-") {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("fragment index removal migration missing")
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/test", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	countIndex := func() int {
		t.Helper()
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='runs_passed_tested_sha_idx'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if countIndex() != 1 {
		t.Fatal("upgrade fixture has no old index")
	}
	for range 2 {
		if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	if countIndex() != 0 {
		t.Fatal("obsolete index survived migration")
	}
	after, err := s.Run(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("run history changed: %v", err)
	}
	afterAudit, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(audit, afterAudit) {
		t.Fatalf("audit history changed: %v", err)
	}
}
