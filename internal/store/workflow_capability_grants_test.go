package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestWorkflowCapabilityGrantsAreScopedAuditedAndRevocable(t *testing.T) {
	ctx := t.Context()
	s, err := openAdminClientForTest(ctx, filepath.Join(t.TempDir(), "grants.sqlite"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	first := createRepo(t, s)
	second, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "second", UpstreamID: first.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	cap := argoworkflow.Capability{Namespace: "pipelines", ServiceAccount: "operator-worker", ResourceKinds: []argoworkflow.ResourceKindCapability{{APIVersion: "example.test/v1", Kind: "Task", Actions: []string{"create"}}}}
	if _, err = s.WorkflowCapabilityGrant(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing grant: %v", err)
	}
	grant, err := s.SetWorkflowCapabilityGrant(ctx, first.ID, "admin", &cap)
	if err != nil {
		t.Fatal(err)
	}
	if grant.RepoID != first.ID || grant.GrantedBy != "admin" || grant.RevokedAt != nil {
		t.Fatalf("grant: %+v", grant)
	}
	cap.ResourceKinds[0].Actions[0] = "delete"
	active, err := s.ActiveWorkflowCapability(ctx, first.ID)
	if err != nil || active == nil || active.ResourceKinds[0].Actions[0] != "create" {
		t.Fatalf("persisted grant changed with caller: %+v %v", active, err)
	}
	if other, e := s.ActiveWorkflowCapability(ctx, second.ID); e != nil || other != nil {
		t.Fatalf("grant leaked across repository: %+v %v", other, e)
	}
	if _, err = s.SetWorkflowCapabilityGrant(ctx, first.ID, "admin", active); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate grant: %v", err)
	}
	if _, err = s.SetWorkflowCapabilityGrant(ctx, first.ID, "admin", &cap); err != nil {
		t.Fatal(err)
	}
	grant, err = s.SetWorkflowCapabilityGrant(ctx, first.ID, "admin", nil)
	if err != nil || grant.RevokedAt == nil {
		t.Fatalf("revoke: %+v %v", grant, err)
	}
	if active, err = s.ActiveWorkflowCapability(ctx, first.ID); err != nil || active != nil {
		t.Fatalf("revoked capability remains active: %+v %v", active, err)
	}
	var count int
	var details string
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM audit_actions WHERE action IN ('workflow_capability.grant','workflow_capability.revoke') AND resource_id=?`, first.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("audit count=%d error=%v", count, err)
	}
	if err = s.db.QueryRow(`SELECT details FROM audit_actions WHERE action='workflow_capability.grant' ORDER BY id DESC LIMIT 1`).Scan(&details); err != nil || !strings.Contains(details, `"previous_capability"`) {
		t.Fatalf("replacement audit: %s %v", details, err)
	}
	// An audit failure must roll back the authority change in the same transaction.
	if _, err = s.db.Exec(`CREATE TRIGGER reject_capability_audit BEFORE INSERT ON audit_actions WHEN NEW.action='workflow_capability.grant' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetWorkflowCapabilityGrant(ctx, second.ID, "admin", &cap); err == nil {
		t.Fatal("grant survived failed audit")
	}
	if active, err = s.ActiveWorkflowCapability(ctx, second.ID); err != nil || active != nil {
		t.Fatalf("failed transaction granted authority: %+v %v", active, err)
	}
}
