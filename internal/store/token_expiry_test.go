package store

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestTokenExpiryMigrationPreservesExistingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := openAdminClientForTest(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, digest := lifecycleUplink(t, database, "legacy", "SHA256:legacy", false)
	if _, err := database.db.ExecContext(t.Context(), integrationDowngradeSQL); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = openAdminClientForTest(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	credential, err := database.TokenCredentialByDigest(t.Context(), digest)
	if err != nil || credential.ExpiresAt != nil {
		t.Fatalf("legacy expiry changed: %+v, %v", credential.ExpiresAt, err)
	}
	if _, err := database.AuthenticatedUplinkByDigest(t.Context(), digest); err != nil {
		t.Fatalf("migration invalidated existing token: %v", err)
	}
}

func TestTokenExpiryValidationAndActivationBoundary(t *testing.T) {
	now := time.Now().UTC()
	database := testStore(t, &now)
	for _, expires := range []time.Time{now, now.Add(-time.Second), time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC)} {
		digest := sha256.Sum256([]byte(expires.String()))
		if _, err := database.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "expiry", Digest: digest[:], ExpiresAt: &expires}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid expiry: %v", err)
		}
	}
	expires := now.Add(time.Second)
	digest := sha256.Sum256([]byte("activation-expiry"))
	credential, err := database.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "expiry", Digest: digest[:], ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RegisterUplink(t.Context(), "operator", model.UplinkSpec{Identity: "expiry", Fingerprint: "SHA256:expiry", TokenCredentialID: credential.ID}); err != nil {
		t.Fatal(err)
	}
	now = expires
	if _, err := database.ActivateTokenCredential(t.Context(), credential.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired pending credential activated: %v", err)
	}
	if _, err := database.AuthenticatedUplinkByDigest(t.Context(), digest[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired pending credential authenticated: %v", err)
	}
}

func TestTokenExpiryCannotSupplyReplacementAdminAuthority(t *testing.T) {
	now := time.Now().UTC()
	database := testStore(t, &now)
	admin, _ := lifecycleUplink(t, database, "operator", "SHA256:active", true)
	expires := now.Add(time.Second)
	digest := sha256.Sum256([]byte("expired-admin"))
	credential, err := database.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "operator", Digest: digest[:], ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	other, err := database.RegisterUplink(t.Context(), "operator", model.UplinkSpec{Identity: "operator", Fingerprint: "SHA256:expired", TokenCredentialID: credential.ID, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ActivateTokenCredential(t.Context(), credential.ID); err != nil {
		t.Fatal(err)
	}
	now = expires
	if _, err := database.AuthenticatedUplinkByDigest(t.Context(), digest[:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired credential authenticated: %v", err)
	}
	if _, err := database.RemoveUplink(t.Context(), "operator", admin.Identity, admin.Fingerprint); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expired credential counted as a remaining admin: %v", err)
	}
	if _, err := database.RevokeUplink(t.Context(), "operator", admin.Identity, admin.Fingerprint, other.Fingerprint); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired replacement accepted: %v", err)
	}
}
