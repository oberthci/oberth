package store

import (
	"errors"
	"testing"
	"time"
)

func TestSecretAccessWorkflowIsolation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := t.Context()
	repo := createRepo(t, s)
	qualified, err := s.QualifiedRepoName(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	const a, b, secret = ".oberth/release.yaml", ".oberth/other.yaml", "oberth/data/release/token"
	first, err := s.Grant(ctx, qualified, a, "publish", secret, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if grants, err := s.ActiveSecretGrants(ctx, repo.ID, b); err != nil || len(grants) != 0 {
		t.Fatal("grant crossed workflow files", grants, err)
	}
	second, err := s.Grant(ctx, qualified, b, "publish", secret, "admin")
	if err != nil || first.ID == second.ID {
		t.Fatal("workflow files share grant identity", first, second, err)
	}
	if _, err := s.Revoke(ctx, qualified, a, "publish", secret, "admin"); err != nil {
		t.Fatal(err)
	}
	if grants, err := s.ActiveSecretGrants(ctx, repo.ID, b); err != nil || !grants["publish"][secret] {
		t.Fatal("revocation crossed workflow files", grants, err)
	}
	if _, err := s.Grant(ctx, qualified, b, "*", secret, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordGrantDeclarations(ctx, qualified, "sha", []GrantDeclaration{{Workflow: a, Step: "publish", Path: secret}, {Workflow: b, Step: "publish", Path: secret}}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingGrantDeclarations(ctx, qualified)
	if err != nil || len(pending) != 1 || pending[0].Workflow != a {
		t.Fatal("wildcard covered another workflow's declaration", pending, err)
	}
	if err := s.DismissGrantDeclaration(ctx, "admin", qualified, b, "publish", secret); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingGrantDeclarations(ctx, qualified)
	if err != nil || len(pending) != 1 || pending[0].Workflow != a {
		t.Fatal("dismiss crossed workflow files", pending, err)
	}
	for _, file := range []string{"", "*", "/release.yaml", "../release.yaml", ".oberth/../release.yaml", "./release.yaml", "release\x00.yaml"} {
		if _, err := s.Grant(ctx, qualified, file, "publish", secret, "admin"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("file %q accepted: %v", file, err)
		}
		if _, err := s.ActiveSecretGrants(ctx, repo.ID, file); !errors.Is(err, ErrInvalid) {
			t.Fatalf("file lookup %q accepted: %v", file, err)
		}
	}
}
