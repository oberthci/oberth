package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestExtendedResourceGrantRemovalPreservesOperatorAuthorityAndHistory(t *testing.T) {
	const id = "20261113010858-remove-extended-resource-grants-1523724f34af80938b4963079df78930"
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, m := range migrations {
		if m.id == id {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("migration missing")
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	raw := `{"namespace":"pipelines","service_account":"operator-worker","resource_kinds":[{"api_version":"example.test/v1","kind":"Task","actions":["create","delete"]}],"extended_resources":{"example.com/gpu":2}}`
	if _, err := s.db.Exec(`INSERT INTO workflow_capability_grants(repo_id,capability_json,granted_by,granted_at,revoked_by,revoked_at) VALUES(?,?,?,123,'',NULL)`, repo.ID, raw, "original-admin"); err != nil {
		t.Fatal(err)
	}
	active, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/running", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(ctx, active.ID, "submitted-workflow"); err != nil {
		t.Fatal(err)
	}
	queued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/queued", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	audit, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	grant, err := s.WorkflowCapabilityGrant(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if grant.GrantedBy != "original-admin" || grant.RevokedAt != nil || grant.Capability.ServiceAccount != "operator-worker" || len(grant.Capability.ResourceKinds) != 1 || !reflect.DeepEqual(grant.Capability.ResourceKinds[0].Actions, []string{"create", "delete"}) {
		t.Fatal("non-device authority changed", grant)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT capability_json FROM workflow_capability_grants WHERE repo_id=?`, repo.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stored), &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["extended_resources"]; exists {
		t.Fatal("retired device ceiling remains", stored)
	}
	receipt, err := s.WorkflowCapabilityRecovery(ctx, repo.ID, active.ID, "submitted-workflow", strings.Repeat("a", 40))
	if err != nil || string(receipt) != raw {
		t.Fatal("original binding not retained for running submission", string(receipt), err)
	}
	if receipt, err = s.WorkflowCapabilityRecovery(ctx, repo.ID, queued.ID, "submitted-workflow", strings.Repeat("b", 40)); err != nil || len(receipt) != 0 {
		t.Fatal("queued work received historical authority", string(receipt), err)
	}
	if _, err = s.db.Exec(`UPDATE workflow_capability_recoveries SET sha='changed' WHERE run_id=?`, active.ID); err == nil {
		t.Fatal("migration receipt is mutable")
	}
	after, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(audit, after) {
		t.Fatal("audit rewritten", err)
	}
}

func TestCapabilityRecoveryDoesNotSurviveSameIDRequeue(t *testing.T) {
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, m := range migrations {
		if m.id == "20261113010858-remove-extended-resource-grants-1523724f34af80938b4963079df78930" {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("grant migration missing")
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	old := `{"namespace":"pipelines","service_account":"operator-worker","extended_resources":{"example.com/gpu":2}}`
	if _, err := s.db.Exec(`INSERT INTO workflow_capability_grants(repo_id,capability_json,granted_by,granted_at) VALUES(?,?,'operator',123)`, repo.ID, old); err != nil {
		t.Fatal(err)
	}
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/requeue", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(ctx, run.ID, "same-name"); err != nil {
		t.Fatal(err)
	}
	if err = s.migrateWithMigrations(ctx, nil, migrations); err != nil {
		t.Fatal(err)
	}
	if receipt, err := s.WorkflowCapabilityRecovery(ctx, repo.ID, run.ID, "same-name", run.SHA); err != nil || len(receipt) == 0 {
		t.Fatal("original receipt missing", string(receipt), err)
	}
	if _, err = s.RequeueStrandedRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteRunCancellation(ctx, run.ID, "same-name"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(ctx, run.ID, "same-name"); err != nil {
		t.Fatal(err)
	}
	if receipt, err := s.WorkflowCapabilityRecovery(ctx, repo.ID, run.ID, "same-name", run.SHA); err != nil || len(receipt) != 0 {
		t.Fatal("replacement attempt revived original receipt", string(receipt), err)
	}
}
