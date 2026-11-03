package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// Retiring execution code must preserve historical evidence without leaving a
// writable authority behind. In particular, an upgrade cannot invent cleanup.
func TestExecutionRetirementPreservesInertHistory(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "execution-history.sqlite")
	s, err := OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "history", SHA: strings.Repeat("a", 40), Actor: "tester", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"vm_executions", "vm_capacity_slots", "kubevirt_grants"} {
		for _, action := range []string{"insert", "update", "delete"} {
			if _, err = s.db.Exec("DROP TRIGGER IF EXISTS " + table + "_retired_" + action); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = s.db.Exec(`DELETE FROM schema_migrations WHERE version>=47`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO vm_executions(run_id,execution_kind,intent_json,spec_identity,workflow_uid,submitted,created_at,updated_at)
		VALUES(?,'kvm-preflight','{"historical":"intent"}','identity','historical-uid',1,1,1)`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO vm_capacity_slots(slot,run_id) VALUES(1,?)`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO kubevirt_grants(repo_id,granted_by,granted_at) VALUES(?,'historical-admin',1)`, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var intent, uid string
	var cleaned int
	if err = s.db.QueryRow(`SELECT intent_json,workflow_uid,cleaned FROM vm_executions WHERE run_id=?`, run.ID).Scan(&intent, &uid, &cleaned); err != nil || intent != `{"historical":"intent"}` || uid != "historical-uid" || cleaned != 0 {
		t.Fatalf("history changed: %q %q %d: %v", intent, uid, cleaned, err)
	}
	for _, table := range []string{"vm_executions", "vm_capacity_slots", "kubevirt_grants"} {
		for _, statement := range []string{"INSERT OR REPLACE INTO " + table + " SELECT * FROM " + table, "DELETE FROM " + table} {
			if _, err = s.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "retired execution history is read-only") {
				t.Errorf("history mutation %q: %v", statement, err)
			}
		}
	}
	for _, statement := range []string{`UPDATE vm_executions SET cleaning=1`, `UPDATE vm_capacity_slots SET slot=1`, `UPDATE kubevirt_grants SET granted_by='changed'`} {
		if _, err = s.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "retired execution history is read-only") {
			t.Errorf("history mutation %q: %v", statement, err)
		}
	}
	if err = s.SetRunCredentialed(ctx, run.ID); err != nil {
		t.Errorf("retired journal still gates ordinary runs: %v", err)
	}
	capability, err := s.ActiveWorkflowCapability(ctx, repo.ID)
	if err != nil || capability != nil {
		t.Fatalf("historical grant became generic authority: %+v, %v", capability, err)
	}
}

const executionRetirementDowngradeSQL = `
DROP TRIGGER IF EXISTS vm_executions_retired_insert;
DROP TRIGGER IF EXISTS vm_executions_retired_update;
DROP TRIGGER IF EXISTS vm_executions_retired_delete;
DROP TRIGGER IF EXISTS vm_capacity_slots_retired_insert;
DROP TRIGGER IF EXISTS vm_capacity_slots_retired_update;
DROP TRIGGER IF EXISTS vm_capacity_slots_retired_delete;
DROP TRIGGER IF EXISTS kubevirt_grants_retired_insert;
DROP TRIGGER IF EXISTS kubevirt_grants_retired_update;
DROP TRIGGER IF EXISTS kubevirt_grants_retired_delete;
DELETE FROM schema_migrations WHERE version>=47;
`
