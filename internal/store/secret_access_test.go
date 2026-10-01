package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestSecretAccessGrantAndCheck(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// No grant exists yet.
	ok, err := s.SecretAccessCheck(ctx, "terraform", "plan", "terraform/credentials")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected no active grant")
	}

	// Grant access.
	grant, err := s.Grant(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost")
	if err != nil {
		t.Fatal(err)
	}
	if grant.Repo != "terraform" || grant.Step != "plan" || grant.Secret != "terraform/credentials" {
		t.Fatalf("unexpected grant: %+v", grant)
	}
	if grant.ApprovedBy != "admin@localhost" {
		t.Fatalf("approved_by = %q", grant.ApprovedBy)
	}
	if grant.RevokedAt != nil {
		t.Fatal("expected nil revoked_at")
	}

	// Check now succeeds.
	ok, err = s.SecretAccessCheck(ctx, "terraform", "plan", "terraform/credentials")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected active grant")
	}

	// Granting the same triple again returns the existing row.
	duplicate, err := s.Grant(ctx, "terraform", "plan", "terraform/credentials", "other-admin")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != grant.ID {
		t.Fatalf("duplicate grant created: got id=%d, want %d", duplicate.ID, grant.ID)
	}
}

func TestSecretAccessRevoke(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	_, err := s.Grant(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost")
	if err != nil {
		t.Fatal(err)
	}

	revoked, err := s.Revoke(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost")
	if err != nil {
		t.Fatal(err)
	}
	if revoked.RevokedBy != "admin@localhost" {
		t.Fatalf("revoked_by = %q", revoked.RevokedBy)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("expected non-nil revoked_at")
	}

	// Check should now return false.
	ok, err := s.SecretAccessCheck(ctx, "terraform", "plan", "terraform/credentials")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected no active grant after revocation")
	}

	// Revoking again should return ErrNotFound.
	_, err = s.Revoke(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost")
	if err == nil {
		t.Fatal("expected error for double revoke")
	}
}

func TestSecretAccessList(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.Grant(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(ctx, "terraform", "apply", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(ctx, "oberth", "release", "cosign-secret", "admin@localhost"); err != nil {
		t.Fatal(err)
	}

	// List active for terraform.
	grants, err := s.SecretAccessList(ctx, "terraform", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(grants))
	}

	// List all repos.
	all, err := s.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 grants, got %d", len(all))
	}

	// Revoke one and list with revoked.
	if _, err := s.Revoke(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	active, err := s.SecretAccessList(ctx, "terraform", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active grant, got %d", len(active))
	}
	withRevoked, err := s.SecretAccessList(ctx, "terraform", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(withRevoked) != 2 {
		t.Fatalf("expected 2 grants (including revoked), got %d", len(withRevoked))
	}
}

func TestSecretAccessGrantAfterRevoke(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	first, err := s.Grant(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revoke(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}

	// Re-granting creates a new row.
	second, err := s.Grant(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("expected a new grant after revoke-and-regrant")
	}

	ok, err := s.SecretAccessCheck(ctx, "terraform", "plan", "terraform/credentials")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected active grant after re-grant")
	}
}

func TestActiveSecretGrants(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Create upstream + repo so QualifiedRepoName can resolve.
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/skipops",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Grants are persisted under the qualified key — the form the v12
	// migration writes and the reconciler/API handlers converge on.
	qualified, err := s.QualifiedRepoName(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(ctx, qualified, "plan", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(ctx, qualified, "plan", "terraform/state", "admin@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(ctx, qualified, "apply", "terraform/credentials", "admin@localhost"); err != nil {
		t.Fatal(err)
	}

	grants, err := s.ActiveSecretGrants(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(grants))
	}
	if !grants["plan"]["terraform/credentials"] {
		t.Fatal("missing plan/terraform/credentials")
	}
	if !grants["plan"]["terraform/state"] {
		t.Fatal("missing plan/terraform/state")
	}
	if !grants["apply"]["terraform/credentials"] {
		t.Fatal("missing apply/terraform/credentials")
	}
}

// TestActiveSecretGrantsBareRowIsInert pins the fail-closed semantics of the
// qualified-only lookup: a stray bare-keyed row (possible only for a repo
// that was unregistered when the v12 migration and the reconciler ran) must
// match NEITHER same-name repository. A bare-name fallback here would grant
// it to both — the #245 BLOCKER B aliasing.
func TestActiveSecretGrantsBareRowIsInert(t *testing.T) {
	now := time.Date(2026, 8, 26, 11, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream1, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/skipops",
	})
	if err != nil {
		t.Fatal(err)
	}
	upstream2, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo1, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream1.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo2, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream2.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A bare-keyed grant row, as a pre-migration deployment would have
	// persisted it.
	if _, err := s.Grant(ctx, "terraform", "*", "release/cosign", "admin@localhost"); err != nil {
		t.Fatal(err)
	}

	for _, repo := range []struct {
		id   int64
		name string
	}{{repo1.ID, "repo1"}, {repo2.ID, "repo2"}} {
		grants, err := s.ActiveSecretGrants(ctx, repo.id)
		if err != nil {
			t.Fatal(err)
		}
		if len(grants) != 0 {
			t.Fatalf("%s: bare row matched %d step(s); bare rows must be inert, never aliased", repo.name, len(grants))
		}
	}
}

// TestActiveSecretGrantsIsolation verifies that same-name repos under
// different upstreams do not share grants (#245 BLOCKER B).
func TestActiveSecretGrantsIsolation(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream1, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/skipops",
	})
	if err != nil {
		t.Fatal(err)
	}
	upstream2, err := s.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo1, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream1.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo2, err := s.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream2.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Grant with qualified name for repo1 only.
	qualified1, err := s.QualifiedRepoName(ctx, repo1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant(ctx, qualified1, "*", "release/cosign", "admin@localhost"); err != nil {
		t.Fatal(err)
	}

	// repo1 sees the grant.
	grants1, err := s.ActiveSecretGrants(ctx, repo1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants1) != 1 {
		t.Fatalf("repo1: expected 1 step, got %d", len(grants1))
	}
	if !grants1["*"]["release/cosign"] {
		t.Fatal("repo1: missing */release/cosign")
	}

	// repo2 must NOT see repo1 grants.
	grants2, err := s.ActiveSecretGrants(ctx, repo2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants2) != 0 {
		t.Fatalf("repo2: expected 0 steps, got %d (grant aliasing!)", len(grants2))
	}
}

func TestGrantAndRevokeAppendAuditActions(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty chain — head ID is 0.
	head, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != 0 {
		t.Fatalf("initial chain head ID = %d, want 0", head.ID)
	}

	// Grant advances chain by exactly 1.
	grant, err := s.Grant(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost")
	if err != nil {
		t.Fatal(err)
	}

	head, err = s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != 1 {
		t.Fatalf("chain head after grant ID = %d, want 1", head.ID)
	}

	// Idempotent grant does NOT advance chain.
	dup, err := s.Grant(ctx, "terraform", "plan", "terraform/credentials", "other-admin")
	if err != nil {
		t.Fatal(err)
	}
	if dup.ID != grant.ID {
		t.Fatalf("idempotent grant ID = %d, want %d", dup.ID, grant.ID)
	}
	head, err = s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != 1 {
		t.Fatalf("chain head after idempotent grant ID = %d, want 1 (no new action)", head.ID)
	}

	// Revoke advances chain by exactly 1 more.
	_, err = s.Revoke(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost")
	if err != nil {
		t.Fatal(err)
	}
	head, err = s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != 2 {
		t.Fatalf("chain head after revoke ID = %d, want 2", head.ID)
	}
}

func TestConcurrentGrantNoSpuriousError(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	type grantResult struct {
		grant SecretAccessGrant
		err   error
	}
	results := make(chan grantResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			g, err := s.Grant(ctx, "terraform", "plan", "terraform/credentials", "admin@localhost")
			results <- grantResult{g, err}
		}()
	}
	var ids []int64
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent Grant error: %v", r.err)
		}
		ids = append(ids, r.grant.ID)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("expected both grants to return the same row, got IDs %v", ids)
	}

	// Exactly one active row.
	active, err := s.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active grant after concurrent Grant, got %d", len(active))
	}
}

func TestSecretAccessCheckValidation(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.SecretAccessCheck(ctx, "", "plan", "terraform/credentials"); err == nil {
		t.Fatal("expected error for empty repo")
	}
	if _, err := s.SecretAccessCheck(ctx, "terraform", "", "terraform/credentials"); err == nil {
		t.Fatal("expected error for empty step")
	}
	if _, err := s.SecretAccessCheck(ctx, "terraform", "plan", ""); err == nil {
		t.Fatal("expected error for empty secret")
	}
}

// --- Grant charset validation tests (issue #414) ---

func TestGrantRejectsHCLInjectionInSecret(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// A secret name containing HCL metacharacters must be rejected at
	// persistence time so hostile values never reach the Vault policy
	// interpolation in the installer.
	hostile := `x" { capabilities = ["sudo"] } path "y`
	_, err := s.Grant(ctx, "terraform", "plan", hostile, "admin@localhost")
	if err == nil {
		t.Fatal("expected rejection of secret containing HCL injection characters")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got: %v", err)
	}
}

func TestGrantRejectsControlCharsInSecret(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	for _, secret := range []string{"path\x00evil", "path\nevil", "path\tevil", `path\evil`} {
		if _, err := s.Grant(ctx, "terraform", "plan", secret, "admin@localhost"); err == nil {
			t.Errorf("Grant(secret=%q) = nil; want error for unsafe characters", secret)
		}
	}
}

func TestGrantAcceptsValidSecret(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	for _, secret := range []string{
		"terraform/credentials",
		"release/cosign-secret",
		"upstream/org/repo/my.secret_name",
	} {
		if _, err := s.Grant(ctx, "terraform", "plan", secret, "admin@localhost"); err != nil {
			t.Errorf("Grant(secret=%q) = %v; want nil", secret, err)
		}
	}
}

// --- Grant repo charset validation tests (issue #416) ---

func TestGrantRejectsHCLInjectionInRepo(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// A repo name containing HCL metacharacters must be rejected at
	// persistence time so hostile values never reach the Vault policy
	// interpolation in the installer.
	hostile := `x" { capabilities = ["sudo"] } path "y`
	_, err := s.Grant(ctx, hostile, "plan", "terraform/credentials", "admin@localhost")
	if err == nil {
		t.Fatal("expected rejection of repo containing HCL injection characters")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got: %v", err)
	}
}

func TestGrantRejectsControlCharsInRepo(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	for _, repo := range []string{"repo\x00evil", "repo\nevil", "repo\tevil", `repo\evil`} {
		if _, err := s.Grant(ctx, repo, "plan", "terraform/credentials", "admin@localhost"); err == nil {
			t.Errorf("Grant(repo=%q) = nil; want error for unsafe characters", repo)
		}
	}
}

func TestGrantAcceptsValidRepo(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	for _, repo := range []string{
		"terraform",
		"codeberg/oberthci/oberth",
		"my.upstream/my-org/repo_name",
	} {
		if _, err := s.Grant(ctx, repo, "plan", "terraform/credentials", "admin@localhost"); err != nil {
			t.Errorf("Grant(repo=%q) = %v; want nil", repo, err)
		}
	}
}
