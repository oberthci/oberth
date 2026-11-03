package store

import (
	"path/filepath"
	"testing"
)

// Stage 2 is deferred: old grants retain their bounded VM authority across an
// upgrade, confer no generic authority and remain explicitly revocable.
func TestLegacyExecutionGrantSurvivesCapabilityUpgrade(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "compatibility.sqlite")
	s, err := OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	if _, err = s.SetKubeVirtGrant(ctx, repo.ID, "admin", true, KubeVirtBudget{}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(executionRetirementDowngradeSQL + `DELETE FROM schema_migrations WHERE version>=47; DROP TABLE workflow_capability_grants`); err != nil {
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
	budget, active, err := s.ActiveKubeVirtGrant(ctx, repo.ID)
	if err != nil || !active || budget.WithDefaults() != DefaultKubeVirtBudget() {
		t.Fatalf("legacy grant lost: %+v %v %v", budget, active, err)
	}
	capability, err := s.ActiveWorkflowCapability(ctx, repo.ID)
	if err != nil || capability != nil {
		t.Fatalf("legacy grant widened to generic authority: %+v %v", capability, err)
	}
	if _, err = s.SetKubeVirtGrant(ctx, repo.ID, "admin", false, KubeVirtBudget{}); err != nil {
		t.Fatal(err)
	}
	_, active, err = s.ActiveKubeVirtGrant(ctx, repo.ID)
	if err != nil || active {
		t.Fatalf("legacy grant cannot be revoked: %v %v", active, err)
	}
}

const executionRetirementDowngradeSQL = resourceCeilingDowngradeSQL + executionRetirementTriggersDowngradeSQL

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
