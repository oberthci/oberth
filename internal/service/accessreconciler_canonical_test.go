package service

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

func grantsConfigMap(rv, grants string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretAccessConfigMapName,
			Namespace:       "oberth",
			ResourceVersion: rv,
		},
		Data: map[string]string{secretAccessConfigMapKey: grants},
	}
}

// TestReconcileCanonicalizesBareEntryOntoQualifiedRow is the regression test
// for the post-v12 converge loop (#245 BLOCKER B follow-up): a bare-spelled
// ConfigMap entry and its v12-qualified sqlite row are the SAME grant. The
// reconciler must resolve the spelling before diffing — without that, every
// converge revoked the qualified row and re-created it bare, undoing the
// migration and reopening bare-key grant aliasing.
func TestReconcileCanonicalizesBareEntryOntoQualifiedRow(t *testing.T) {
	cm := grantsConfigMap("7", `- {repo: terraform, step: "*", secret: oberth/data/release/cosign-secret}
`)
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	qualified, err := database.QualifiedRepoName(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The v12 migration's post-state: the grant persisted under the
	// qualified key.
	if _, err := database.Grant(ctx, qualified, "*", "oberth/data/release/cosign-secret", "migration-v12"); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected exactly 1 active grant after converge, got %d: %+v", len(grants), grants)
	}
	if grants[0].Repo != qualified {
		t.Fatalf("grant repo = %q, want qualified %q (bare converge would undo the v12 migration)", grants[0].Repo, qualified)
	}
	// The original migration-authored row must have survived untouched —
	// not been revoked and re-created by the converge.
	if grants[0].ApprovedBy != "migration-v12" {
		t.Fatalf("grant approved_by = %q, want migration-v12 (row was churned by the converge)", grants[0].ApprovedBy)
	}

	// The qualified row is what admission consults.
	active, err := database.ActiveSecretGrants(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !active["*"]["oberth/data/release/cosign-secret"] {
		t.Fatal("admission lost the grant after converge")
	}
}

// TestReconcileSkipsAmbiguousBareEntryFailClosed pins the ambiguity
// semantics: when two same-name repositories exist under different
// upstreams, a bare-spelled ConfigMap entry names neither of them and must
// grant to neither. Qualified entries keep working.
func TestReconcileSkipsAmbiguousBareEntryFailClosed(t *testing.T) {
	cm := grantsConfigMap("9", `- {repo: terraform, step: "*", secret: oberth/data/release/cosign-secret}
- {repo: codeberg/acme/terraform, step: "*", secret: oberth/data/release/r2-upload-token}
`)
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	upstream1, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	upstream2, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "skipops", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/skipops",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo1, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream1.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo2, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream2.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// The ambiguous bare entry granted nothing; the qualified entry
	// granted exactly its named repository.
	grants1, err := database.ActiveSecretGrants(ctx, repo1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if grants1["*"]["oberth/data/release/cosign-secret"] {
		t.Fatal("ambiguous bare entry granted to repo1 (must be skipped fail-closed)")
	}
	if !grants1["*"]["oberth/data/release/r2-upload-token"] {
		t.Fatal("qualified entry did not grant to its named repository")
	}
	grants2, err := database.ActiveSecretGrants(ctx, repo2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants2) != 0 {
		t.Fatalf("repo2 received %d grant step(s) from an ambiguous or foreign entry", len(grants2))
	}
}

// TestRemoveGrantCanonicalBareEntry verifies that a qualified revoke matches
// a bare-spelled ConfigMap entry via canonical identity resolution (#689).
func TestRemoveGrantCanonicalBareEntry(t *testing.T) {
	cm := grantsConfigMap("20", `- {repo: acme-beacon, step: "*", secret: oberth/data/release/gar-sa-key}
`)
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "acme-beacon", UpstreamID: upstream.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	// Initial reconcile.
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant before revoke, got %d", len(grants))
	}

	// Revoke using the qualified name — must match the bare-spelled entry.
	qualified := "codeberg/acme/acme-beacon"
	if err := reconciler.UpdateConfigMap(ctx, "admin@test",
		reconciler.RemoveGrantCanonical(ctx, qualified, "*", "oberth/data/release/gar-sa-key")); err != nil {
		t.Fatalf("RemoveGrantCanonical failed: %v", err)
	}

	// The grant should be revoked.
	grants, err = database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("expected 0 active grants after canonical revoke, got %d", len(grants))
	}
}

// TestRemoveGrantCanonicalBareAndQualifiedDuplicates verifies that when both
// a bare-spelled and a qualified entry exist for the same canonical tuple,
// both are removed by a single revoke.
func TestRemoveGrantCanonicalBareAndQualifiedDuplicates(t *testing.T) {
	cm := grantsConfigMap("21", `- {repo: acme-beacon, step: "*", secret: oberth/data/release/gar-sa-key}
- {repo: codeberg/acme/acme-beacon, step: "*", secret: oberth/data/release/gar-sa-key}
- {repo: codeberg/acme/acme-beacon, step: "*", secret: oberth/data/release/r2-upload-token}
`)
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "acme-beacon", UpstreamID: upstream.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	qualified := "codeberg/acme/acme-beacon"
	if err := reconciler.UpdateConfigMap(ctx, "admin@test",
		reconciler.RemoveGrantCanonical(ctx, qualified, "*", "oberth/data/release/gar-sa-key")); err != nil {
		t.Fatalf("RemoveGrantCanonical failed: %v", err)
	}

	// Only the r2-upload-token grant should survive.
	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 active grant after duplicate removal, got %d", len(grants))
	}
	if grants[0].Secret != "oberth/data/release/r2-upload-token" {
		t.Fatalf("unexpected surviving grant: %+v", grants[0])
	}
}

// TestRemoveGrantCanonicalAmbiguousFailsClosed verifies that when a bare
// repo name is ambiguous (same name under two upstreams), the ambiguous
// entry is not matched by the revoke and the removal fails closed.
func TestRemoveGrantCanonicalAmbiguousFailsClosed(t *testing.T) {
	// ConfigMap has only a bare entry for "terraform".
	cm := grantsConfigMap("22", `- {repo: terraform, step: "*", secret: oberth/data/release/cosign-secret}
`)
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	// Create two same-name repos under different upstreams.
	upstream1, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	upstream2, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "skipops", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/skipops",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream1.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream2.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// Try to revoke as one of the qualified names — the bare entry is
	// ambiguous and must not match either, so the revoke should fail.
	err = reconciler.UpdateConfigMap(ctx, "admin@test",
		reconciler.RemoveGrantCanonical(ctx, "codeberg/acme/terraform", "*", "oberth/data/release/cosign-secret"))
	if err == nil {
		t.Fatal("expected error for ambiguous bare entry, got nil")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got: %v", err)
	}
}

// TestRemoveGrantCanonicalAbsentReturnsInvalidInput verifies that removing a
// truly absent canonical tuple returns ErrInvalidInput, not an internal error.
func TestRemoveGrantCanonicalAbsentReturnsInvalidInput(t *testing.T) {
	cm := grantsConfigMap("23", `- {repo: oberth, step: "*", secret: oberth/data/release/cosign-secret}
`)
	reconciler, _ := testAccessReconciler(t, cm)
	ctx := context.Background()

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	err := reconciler.UpdateConfigMap(ctx, "admin@test",
		reconciler.RemoveGrantCanonical(ctx, "nonexistent", "*", "nonexistent/secret"))
	if err == nil {
		t.Fatal("expected error for absent grant")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got: %v", err)
	}
}

// TestAddGrantCanonicalDedupeAgainstBareEntry verifies that a qualified add
// beside an existing bare entry of the same canonical repo is a no-op.
func TestAddGrantCanonicalDedupeAgainstBareEntry(t *testing.T) {
	cm := grantsConfigMap("24", `- {repo: acme-beacon, step: "*", secret: oberth/data/release/gar-sa-key}
`)
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "acme-beacon", UpstreamID: upstream.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// Adding the same grant under its qualified name should be a no-op.
	qualified := "codeberg/acme/acme-beacon"
	if err := reconciler.UpdateConfigMap(ctx, "admin@test",
		reconciler.AddGrantCanonical(ctx, qualified, "*", "oberth/data/release/gar-sa-key")); err != nil {
		t.Fatalf("AddGrantCanonical failed: %v", err)
	}

	// Still exactly one active grant (not a duplicate).
	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 grant after canonical dedupe, got %d", len(grants))
	}

	// Verify the ConfigMap was not doubled.
	cmAfter, err := reconciler.client.CoreV1().ConfigMaps("oberth").Get(ctx, secretAccessConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ParseGrants(cmAfter.Data[secretAccessConfigMapKey])
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 ConfigMap entry after canonical dedupe, got %d", len(entries))
	}
}

// TestReconcileKeepsUnregisteredEntryInert: a grant entry for a repository
// that has not been discovered yet stays spelled as-is and is invisible to
// ID-keyed admission until the repo registers and the entry resolves.
func TestReconcileKeepsUnregisteredEntryInert(t *testing.T) {
	cm := grantsConfigMap("11", `- {repo: not-yet-pushed, step: "*", secret: oberth/data/release/cosign-secret}
`)
	reconciler, database := testAccessReconciler(t, cm)
	ctx := context.Background()

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	grants, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Repo != "not-yet-pushed" {
		t.Fatalf("unregistered entry not kept verbatim: %+v", grants)
	}

	// Once the repository registers, the next converge canonicalizes the
	// entry onto the qualified key.
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "not-yet-pushed", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	active, err := database.ActiveSecretGrants(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !active["*"]["oberth/data/release/cosign-secret"] {
		t.Fatal("entry did not converge onto the qualified key after registration")
	}
	// And the stale bare row was revoked by the same converge.
	all, err := database.SecretAccessList(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	activeCount := 0
	for _, grant := range all {
		if grant.RevokedAt == nil {
			activeCount++
			if grant.Repo == "not-yet-pushed" {
				t.Fatalf("stale bare row still active after registration converge: %+v", grant)
			}
		}
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly 1 active grant after registration converge, got %d", activeCount)
	}
}
