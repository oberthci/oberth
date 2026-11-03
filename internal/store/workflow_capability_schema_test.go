package store

import (
	"path/filepath"
	"testing"
)

func TestWorkflowCapabilitiesStartEmptyAcrossUpgrade(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "capabilities.sqlite")
	s, err := OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	if _, err = s.db.Exec(executionRetirementDowngradeSQL+`DROP TABLE IF EXISTS workflow_capability_grants; DELETE FROM schema_migrations WHERE version>=47; INSERT INTO kubevirt_grants(repo_id,granted_by,granted_at) VALUES(?,'historical-admin',1)`, repo.ID); err != nil {
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
	var n int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM workflow_capability_grants`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("new capability authority must start empty, including upgraded installs: count=%d error=%v", n, err)
	}
	for _, statement := range []string{
		`INSERT INTO workflow_capability_grants(repo_id,capability_json,granted_by,granted_at) VALUES(999999,'{}','admin',1)`,
		`INSERT INTO workflow_capability_grants(repo_id,capability_json,granted_by,granted_at) VALUES(1,'invalid','admin',1)`,
	} {
		if _, err = s.db.Exec(statement); err == nil {
			t.Fatalf("invalid capability row accepted: %s", statement)
		}
	}
}
