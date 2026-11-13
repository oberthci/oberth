package store

import (
	"reflect"
	"strings"
	"testing"
)

const recoveryUIDMigrationID = "20261113054824-bind-workflow-capability-recovery-uid-3d13e105c1264e33bbf137990e3e1977"

// This is trusted test fixture setup, not a production issuer or UID backfill.
func seedCapabilityRecoveryUID(t *testing.T, s *Store, run string, repo int64, name, uid, sha, original string) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO workflow_capability_recoveries
 (run_id,repo_id,workflow_name,workflow_uid,sha,capability_json,projected_json,granted_by,granted_at)
 SELECT ?,repo_id,?,?,?,?,capability_json,granted_by,granted_at
 FROM workflow_capability_grants WHERE repo_id=?`, run, name, uid, sha, original, repo)
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowCapabilityUIDMigrationDiscardsUnboundReceipts(t *testing.T) {
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, m := range migrations {
		if m.id == recoveryUIDMigrationID {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("UID migration missing")
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/uid", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := s.SetRunWorkflowName(ctx, run.ID, "original-workflow")
	if err != nil {
		t.Fatal(err)
	}
	current := `{"namespace":"pipelines","service_account":"worker"}`
	old := `{"namespace":"pipelines","service_account":"worker","extended_resources":{"example.com/gpu":1}}`
	if _, err = s.db.Exec(`INSERT INTO workflow_capability_grants(repo_id,capability_json,granted_by,granted_at) VALUES(?,?,'operator',123)`, repo.ID, current); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO workflow_capability_recoveries VALUES(?,?,?,?,?,?,?,?)`, run.ID, repo.ID, "original-workflow", run.SHA, old, current, "operator", 123); err != nil {
		t.Fatal(err)
	}
	audit, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM workflow_capability_recoveries`).Scan(&count); err != nil || count != 0 {
		t.Fatal("unbound authority retained", count, err)
	}
	after, err := s.Run(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("run changed", before, after, err)
	}
	afterAudit, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(audit, afterAudit) {
		t.Fatal("audit changed", err)
	}
	var projection string
	if err = s.db.QueryRow(`SELECT capability_json FROM workflow_capability_grants WHERE repo_id=?`, repo.ID).Scan(&projection); err != nil || projection != current {
		t.Fatal("current grant changed", projection, err)
	}
	for _, uid := range []string{"", "   "} {
		if _, err = s.db.Exec(`INSERT INTO workflow_capability_recoveries VALUES(?,?,?,?,?,?,?,?,?)`, run.ID, repo.ID, "original-workflow", uid, run.SHA, old, current, "operator", 123); err == nil {
			t.Fatal("empty UID accepted")
		}
	}
	seedCapabilityRecoveryUID(t, s, run.ID, repo.ID, "original-workflow", "original-uid", run.SHA, old)
	for _, uid := range []string{"original-uid", "replacement-uid", "", "   "} {
		got, err := s.WorkflowCapabilityRecovery(ctx, repo.ID, run.ID, "original-workflow", uid, run.SHA)
		if err != nil || (len(got) > 0) != (uid == "original-uid") {
			t.Fatal("UID binding", uid, string(got), err)
		}
	}
	if _, err = s.db.Exec(`UPDATE workflow_capability_recoveries SET workflow_uid='replacement-uid' WHERE run_id=?`, run.ID); err == nil {
		t.Fatal("receipt UID mutable")
	}
}
