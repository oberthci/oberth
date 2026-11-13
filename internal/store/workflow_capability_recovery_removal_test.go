package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestUnusedCapabilityRecoveryRemovalPreservesRunsGrantsAndAudit(t *testing.T) {
	for _, uid := range []bool{false, true} {
		t.Run(map[bool]string{false: "v0.22.0 UID-less receipt", true: "candidate UID receipt"}[uid], func(t *testing.T) {
			s := newMigrationIDTestStore(t)
			index := -1
			suffix := "bind-workflow-capability-recovery-uid"
			if uid {
				suffix = "remove-unused-capability-recovery"
			}
			for i, m := range migrations {
				if strings.Contains(m.id, suffix) {
					index = i
				}
			}
			if index < 0 {
				t.Fatal("missing migration", suffix)
			}
			ctx := t.Context()
			if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
				t.Fatal(err)
			}
			if err := s.configureLiveReaders(ctx, 0); err != nil {
				t.Fatal(err)
			}
			repo := createRepo(t, s)
			run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/recovery", strings.Repeat("a", 40)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ClaimNextRun(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := s.SetRunWorkflowName(ctx, run.ID, "old-workflow")
			if err != nil {
				t.Fatal(err)
			}
			grant := &argoworkflow.Capability{Namespace: "pipelines", ServiceAccount: "operator-worker"}
			if _, err = s.SetWorkflowCapabilityGrant(ctx, repo.ID, "operator", grant); err != nil {
				t.Fatal(err)
			}
			columns, values := "", ""
			if uid {
				columns = ",workflow_uid"
				values = ",'original-uid'"
			}
			_, err = s.db.Exec(`INSERT INTO workflow_capability_recoveries(run_id,repo_id,workflow_name,sha,capability_json,projected_json,granted_by,granted_at`+columns+`) SELECT ?,repo_id,?,?,capability_json,capability_json,granted_by,granted_at`+values+` FROM workflow_capability_grants WHERE repo_id=?`, run.ID, "old-workflow", run.SHA, repo.ID)
			if err != nil {
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
			var remaining int
			if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('workflow_capability_recoveries','workflow_capability_recoveries_immutable','workflow_capability_recovery_run_changed')`).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatal("recovery schema remains", remaining, err)
			}
			after, err := s.Run(ctx, run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("run/evidence changed", before, after, err)
			}
			afterAudit, err := s.VerifyAuditChain(ctx)
			if err != nil || !reflect.DeepEqual(audit, afterAudit) {
				t.Fatal("audit changed", err)
			}
			current, err := s.ActiveWorkflowCapability(ctx, repo.ID)
			if err != nil || !reflect.DeepEqual(current, grant) {
				t.Fatal("grant changed", current, err)
			}
			// Dropping the receipt table must also drop the trigger attached to runs.
			if _, err = s.db.Exec(`UPDATE runs SET tested_sha=? WHERE id=?`, strings.Repeat("b", 40), run.ID); err != nil {
				t.Fatal("run trigger still references dropped table", err)
			}
			if _, err = s.SetWorkflowCapabilityGrant(ctx, repo.ID, "operator", nil); err != nil {
				t.Fatal("grant revocation references dropped table", err)
			}
		})
	}
}
