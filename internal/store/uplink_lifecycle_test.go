package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func lifecycleUplink(t *testing.T, s *Store, identity, fingerprint string, admin bool) (model.Uplink, []byte) {
	t.Helper()
	ctx := context.Background()
	digest := sha256.Sum256([]byte(fingerprint))
	credential, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: identity, Digest: digest[:]})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := s.RegisterUplink(ctx, "operator", model.UplinkSpec{Identity: identity, Fingerprint: fingerprint, TokenCredentialID: credential.ID, Admin: admin})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	return uplink, digest[:]
}

func TestRemoteUplinkRevokeAndRotation(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	admin, _ := lifecycleUplink(t, s, "operator", "SHA256:admin", true)
	if _, err := s.RevokeUplink(ctx, "operator", admin.Identity, admin.Fingerprint, ""); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("last admin revocation: %v", err)
	}
	old, digest := lifecycleUplink(t, s, "worker", "SHA256:old", false)
	next, nextDigest := lifecycleUplink(t, s, "worker", "SHA256:new", false)
	if _, err := s.RevokeUplink(ctx, "operator", old.Identity, old.Fingerprint, admin.Fingerprint); err == nil {
		t.Fatal("rotated across identity")
	}
	if _, err := s.AuthenticatedUplinkByDigest(ctx, digest); err != nil {
		t.Fatal("failed rotation revoked original", err)
	}
	if _, err := s.RevokeUplink(ctx, "operator", old.Identity, old.Fingerprint, next.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticatedUplinkByDigest(ctx, digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked bearer authenticates: %v", err)
	}
	if _, err := s.UplinkByFingerprint(ctx, old.Fingerprint); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked SSH authenticates: %v", err)
	}
	if _, err := s.AuthenticatedUplinkByDigest(ctx, nextDigest); err != nil {
		t.Fatal(err)
	}
	statuses, err := s.ListUplinkStatuses(ctx)
	if err != nil || len(statuses) != 3 {
		t.Fatalf("statuses=%+v %v", statuses, err)
	}
	found := false
	for _, status := range statuses {
		if status.Fingerprint == old.Fingerprint {
			found = status.RevokedAt != nil
		}
	}
	if !found {
		t.Fatal("revoked credential absent from listing")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE action='uplink.rotate' AND actor='operator'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit %d %v", count, err)
	}
}

func TestGrantDeclarationSnapshotAndDismiss(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	repo := "forge/org/repo"
	if err := s.RecordGrantDeclarations(ctx, repo, "old", []GrantDeclaration{{Step: "old", Path: "oberth/data/old"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordGrantDeclarations(ctx, "other/org/repo", "old", []GrantDeclaration{{Step: "keep", Path: "oberth/data/keep"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceGrantDeclarations(ctx, repo, "new", []GrantDeclaration{{Step: "ci", Path: "oberth/data/ci"}, {Step: "release", Path: "oberth/data/release"}}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingGrantDeclarations(ctx, repo)
	if err != nil || len(pending) != 2 || pending[0].LastSeenSHA != "new" {
		t.Fatalf("pending=%+v %v", pending, err)
	}
	if err := s.DismissGrantDeclaration(ctx, "admin", repo, "ci", "oberth/data/ci"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceGrantDeclarations(ctx, repo, "empty", nil); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingGrantDeclarations(ctx, "")
	if err != nil || len(pending) != 1 || pending[0].Repo != "other/org/repo" {
		t.Fatalf("pending=%+v %v", pending, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE action='secret_access.dismiss'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit %d %v", count, err)
	}
}

func TestRemoteUplinkRevokesPendingCredential(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	digest := sha256.Sum256([]byte("pending-fixture"))
	credential, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "pending", Digest: digest[:]})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := s.RegisterUplink(ctx, "operator", model.UplinkSpec{Identity: "pending", Fingerprint: "SHA256:pending", TokenCredentialID: credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeUplink(ctx, "operator", uplink.Identity, uplink.Fingerprint, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(ctx, credential.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked pending credential activated: %v", err)
	}
	if _, err := s.UplinkByFingerprint(ctx, uplink.Fingerprint); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked pending SSH credential accepted: %v", err)
	}
}

func TestRemoteRotationRejectsAuthorityMismatch(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	old, digest := lifecycleUplink(t, s, "worker", "SHA256:old", false)
	admin, _ := lifecycleUplink(t, s, "worker", "SHA256:admin", true)
	if _, err := s.RevokeUplink(ctx, "operator", old.Identity, old.Fingerprint, admin.Fingerprint); !errors.Is(err, ErrInvalid) {
		t.Fatalf("admin mismatch: %v", err)
	}
	next, _ := lifecycleUplink(t, s, "worker", "SHA256:next", false)
	if _, err := s.SetUplinkScope(ctx, "operator", next.Identity, next.Fingerprint, []string{"github"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeUplink(ctx, "operator", old.Identity, old.Fingerprint, next.Fingerprint); !errors.Is(err, ErrInvalid) {
		t.Fatalf("scope mismatch: %v", err)
	}
	if _, err := s.AuthenticatedUplinkByDigest(ctx, digest); err != nil {
		t.Fatalf("failed rotation changed old token: %v", err)
	}
}

func TestDeclarationSnapshotRetiresOnlyUnambiguousLegacyNames(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "forge", Kind: "ssh", BaseURL: "ssh://forge.example/org"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	old := []GrantDeclaration{{Step: "old", Path: "oberth/data/old"}}
	if err := s.RecordGrantDeclarations(ctx, "repo", "old", old); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceGrantDeclarations(ctx, "forge/org/repo", "new", nil); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingGrantDeclarations(ctx, "")
	if err != nil || len(pending) != 0 {
		t.Fatalf("legacy pending=%+v %v", pending, err)
	}
	other, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "other", Kind: "ssh", BaseURL: "ssh://other.example/otherorg"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: other.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordGrantDeclarations(ctx, "repo", "old", old); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceGrantDeclarations(ctx, "forge/org/repo", "new", nil); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingGrantDeclarations(ctx, "")
	if err != nil || len(pending) != 1 {
		t.Fatalf("ambiguous legacy pending=%+v %v", pending, err)
	}
}

func TestRemoteUplinkCanRevokeInactiveAdmin(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	digest := sha256.Sum256([]byte("inactive-admin-fixture"))
	credential, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "pending-admin", Digest: digest[:]})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := s.RegisterUplink(ctx, "operator", model.UplinkSpec{Identity: "pending-admin", Fingerprint: "SHA256:pending-admin", TokenCredentialID: credential.ID, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeUplink(ctx, "operator", uplink.Identity, uplink.Fingerprint, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(ctx, credential.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked pending administrator activated: %v", err)
	}
}
