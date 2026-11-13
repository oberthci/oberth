package store

import (
	"encoding/json"
	"reflect"
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
	after, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(audit, after) {
		t.Fatal("audit rewritten", err)
	}
}
