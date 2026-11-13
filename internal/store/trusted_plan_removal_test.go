package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestTrustedPlanRemovalPreservesRunHistory(t *testing.T) {
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, migration := range migrations {
		if strings.Contains(migration.id, "-remove-trusted-plan-tables-") {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatal("plan removal migration missing")
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run := projectionRun(t, s, repo.ID, "feature/plan-removal", projectionSourceSHA)
	digest := strings.Repeat("a", 64)
	if _, err := s.db.Exec(`INSERT INTO trusted_plans(id,repo_id,upstream_id,source_ref,source_sha,target_ref,base_sha,result_sha,green_run_id,plan_run_id,backend_identity,backend_key,tool_digest,lock_digest,config_digest,actor,status,created_at,expires_at,updated_at)
 VALUES('retired-plan',?,?,'feature/plan-removal',?,'main',?,?,?,?,'backend','backend-key',?,?,?,'operator','authorized',1,2,1)`, repo.ID, repo.UpstreamID, run.SHA, run.SHA, run.SHA, run.ID, run.ID, digest, digest, digest); err != nil {
		t.Fatal(err)
	}
	before, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"trusted_plans", "trusted_applies"} {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name=? OR tbl_name=?`, table, table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("retired schema %s: count=%d err=%v", table, count, err)
		}
	}
	retained, err := s.Run(ctx, run.ID)
	if err != nil || retained.SHA != run.SHA {
		t.Fatalf("run history changed: %+v %v", retained, err)
	}
	after, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("audit chain changed: before=%+v after=%+v err=%v", before, after, err)
	}
}
