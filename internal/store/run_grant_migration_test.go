package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestDropRunCredentialedPreservesRunsAndAuditClearsApprovals(t *testing.T) {
	const id = "20261113193452-remove-run-credentialed-14b11551632068123f2deef9e27ca7dc"
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
	run, err := s.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1", SHA: strings.Repeat("a", 40), TestedSHA: strings.Repeat("b", 40), Actor: "author", Release: true, Trigger: "tag"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE runs SET credentialed=1 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	qualified, err := s.QualifiedRepoName(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO secret_access(repo,step,secret,approved_by,approved_at) VALUES(?, 'publish','oberth/data/release/token','admin',1)`, qualified); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "admin", Action: "secret_access.grant", ResourceType: "secret_access", ResourceID: "1", Details: `{"repo":"pre-scope","step":"publish"}`}); err != nil {
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
	if s.AppliedStartupMigration(WorkflowGrantScopeMigrationID) {
		t.Fatal("reset reported before migration ran")
	}
	broken := append([]migration(nil), migrations...)
	for i := range broken {
		if broken[i].ID() == WorkflowGrantScopeMigrationID {
			broken[i].sql = "INVALID SQL"
		}
	}
	if err := s.migrateWithMigrations(ctx, nil, broken); err == nil {
		t.Fatal("broken migration unexpectedly succeeded")
	}
	if s.AppliedStartupMigration(WorkflowGrantScopeMigrationID) {
		t.Fatal("failed migration falsely reports reset")
	}
	for range 2 {
		if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	if !s.AppliedStartupMigration(WorkflowGrantScopeMigrationID) {
		t.Fatal("committed reset not reported")
	}
	after, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("run identity/history changed: before=%+v after=%+v", before, after)
	}
	currentGrants, err := s.ActiveSecretGrants(ctx, repo.ID, ".oberth/release.yaml")
	if err != nil || len(currentGrants) != 0 {
		t.Fatal("pre-scope approvals survived", currentGrants, err)
	}
	currentAudit, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(audit, currentAudit) {
		t.Fatal("audit chain changed", err)
	}
	var columns int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('runs') WHERE name='credentialed'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatal("retired column remains", columns, err)
	}
	body, err := json.Marshal(after)
	if err != nil || strings.Contains(string(body), "Credentialed") {
		t.Fatal("retired API field remains", string(body), err)
	}
}
