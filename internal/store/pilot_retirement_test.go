package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testdb"
)

const pilotRetirementDowngradeSQL = executionRetirementTriggersDowngradeSQL + testdb.PostV38Downgrade

func TestPilotRetirementPreservesReadOnlyHistory(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "pilot-history.sqlite")
	s, err := openAdminClientForTest(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "history", SHA: strings.Repeat("a", 40), Actor: "tester", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(pilotRetirementDowngradeSQL); err != nil {
		t.Fatal(err)
	}
	const plan = `{"historical":"pilot plan"}`
	const intent = `{"historical":"resource intent"}`
	if _, err = s.db.Exec(`INSERT INTO vm_suite_executions(run_id,plan_json,plan_identity,submitted,created_at,updated_at) VALUES(?,?,?,1,1,1)`, run.ID, plan, "identity"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO vm_suite_resources(run_id,resource_key,intent_json) VALUES(?,'resource',?)`, run.ID, intent); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openAdminClientForTest(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var gotPlan, gotIntent string
	if err = s.db.QueryRow(`SELECT plan_json,intent_json FROM vm_suite_executions JOIN vm_suite_resources USING(run_id) WHERE run_id=?`, run.ID).Scan(&gotPlan, &gotIntent); err != nil || gotPlan != plan || gotIntent != intent {
		t.Fatalf("historical records: plan=%q intent=%q error=%v", gotPlan, gotIntent, err)
	}
	for _, query := range []string{
		`INSERT OR REPLACE INTO vm_suite_executions SELECT * FROM vm_suite_executions`,
		`UPDATE vm_suite_executions SET plan_json='changed'`,
		`DELETE FROM vm_suite_executions`,
		`INSERT OR REPLACE INTO vm_suite_resources SELECT * FROM vm_suite_resources`,
		`UPDATE vm_suite_resources SET intent_json='changed'`,
		`DELETE FROM vm_suite_resources`,
	} {
		if _, err = s.db.Exec(query); err == nil || !strings.Contains(err.Error(), "retired pilot history is read-only") {
			t.Errorf("historical mutation %q: %v", query, err)
		}
	}
	// Historical pilot rows cannot participate in current execution decisions.
	if err = s.SetRunCredentialed(ctx, run.ID); err != nil {
		t.Errorf("credential transition consulted retired pilot history: %v", err)
	}
}

const executionRetirementTriggersDowngradeSQL = `
DROP TRIGGER IF EXISTS vm_executions_retired_insert;
DROP TRIGGER IF EXISTS vm_executions_retired_update;
DROP TRIGGER IF EXISTS vm_executions_retired_delete;
DROP TRIGGER IF EXISTS vm_capacity_slots_retired_insert;
DROP TRIGGER IF EXISTS vm_capacity_slots_retired_update;
DROP TRIGGER IF EXISTS vm_capacity_slots_retired_delete;
DROP TRIGGER IF EXISTS kubevirt_grants_retired_insert;
DROP TRIGGER IF EXISTS kubevirt_grants_retired_update;
DROP TRIGGER IF EXISTS kubevirt_grants_retired_delete;
DELETE FROM schema_migrations WHERE version>=48;
`

const executionRetirementDowngradeSQL = resourceCeilingDowngradeSQL + executionRetirementTriggersDowngradeSQL

const resourceCeilingDowngradeSQL = testdb.PostV49Downgrade + `DROP TABLE IF EXISTS resource_ceilings; DELETE FROM schema_migrations WHERE version>=49;`
