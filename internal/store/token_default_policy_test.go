package store

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestNewUplinkTokenDefaultsToNinetyDays(t *testing.T) {
	now := time.Now().UTC()
	database := testStore(t, &now)
	digest := sha256.Sum256([]byte("default-lifetime"))
	credential, err := database.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "default", Digest: digest[:]})
	if err != nil {
		t.Fatal(err)
	}
	if credential.ExpiresAt == nil || !credential.ExpiresAt.Equal(now.Add(90*24*time.Hour)) {
		t.Fatalf("new token expiry = %v, want 90 days", credential.ExpiresAt)
	}
}

func TestNewUplinkTokenRejectsLifetimeAboveMaximum(t *testing.T) {
	now := time.Now().UTC()
	database := testStore(t, &now)
	digest := sha256.Sum256([]byte("excessive-lifetime"))
	expires := now.Add(366 * 24 * time.Hour)
	if _, err := database.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "too-long", Digest: digest[:], ExpiresAt: &expires}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("above-maximum lifetime accepted: %v", err)
	}
}

func TestConfiguredTokenPolicyUsesDefaultAndEnforcesExactMaximum(t *testing.T) {
	now := time.Now().UTC()
	database := testStore(t, &now)
	policy := model.UplinkTokenPolicy{DefaultTTL: 30 * 24 * time.Hour, MaxTTL: 60 * 24 * time.Hour}
	for _, tc := range []struct {
		name  string
		delta time.Duration
		valid bool
	}{
		{"default", 0, true}, {"maximum", policy.MaxTTL, true}, {"above-maximum", policy.MaxTTL + time.Nanosecond, false},
	} {
		digest := sha256.Sum256([]byte(tc.name))
		spec := model.TokenCredentialSpec{Name: tc.name, Digest: digest[:]}
		if tc.delta > 0 {
			expires := now.Add(tc.delta)
			spec.ExpiresAt = &expires
		}
		credential, err := database.CreateTokenCredentialWithPolicy(t.Context(), spec, policy)
		if !tc.valid {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if _, err := database.TokenCredentialByDigest(t.Context(), digest[:]); !errors.Is(err, ErrNotFound) {
				t.Fatalf("refused credential persisted: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := tc.delta
		if want == 0 {
			want = policy.DefaultTTL
		}
		if credential.ExpiresAt == nil || !credential.ExpiresAt.Equal(now.Add(want)) {
			t.Fatalf("%s expiry = %v", tc.name, credential.ExpiresAt)
		}
	}
}

func TestLegacyTokenWarningsAreReadOnlyAndExcludeInactiveCredentials(t *testing.T) {
	now := time.Now().UTC()
	database := testStore(t, &now)
	legacy, digest := lifecycleUplink(t, database, "legacy", "SHA256:legacy", false)
	revoked, _ := lifecycleUplink(t, database, "revoked", "SHA256:revoked", false)
	_, _ = lifecycleUplink(t, database, "bounded", "SHA256:bounded", false)
	if _, err := database.db.ExecContext(t.Context(), `UPDATE token_credentials SET expires_at=NULL WHERE id IN (?,?)`, legacy.TokenCredentialID, revoked.TokenCredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RevokeUplink(t.Context(), "operator", revoked.Identity, revoked.Fingerprint, ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		count, err := database.CountNonExpiringUplinkTokens(t.Context())
		if err != nil || count != 1 {
			t.Fatalf("legacy count=%d, %v", count, err)
		}
		if _, err := database.AuthenticatedUplinkByDigest(t.Context(), digest); err != nil {
			t.Fatalf("legacy authentication changed: %v", err)
		}
	}
	credential, err := database.TokenCredentialByDigest(t.Context(), digest)
	if err != nil || credential.ExpiresAt != nil {
		t.Fatalf("warning retroactively assigned expiry: %v, %v", credential.ExpiresAt, err)
	}
}

func TestAdminTokenExpiryWarnsInsideTwoWeeks(t *testing.T) {
	now := time.Now().UTC()
	database := testStore(t, &now)
	if expiry, err := database.AdminTokenExpiry(t.Context()); err != nil || expiry != nil {
		t.Fatalf("no admin: %+v, %v", expiry, err)
	}
	// The default 90-day admin token: visible, no warning yet.
	admin, _ := lifecycleUplink(t, database, "admin", "SHA256:admin", true)
	scoped, _ := lifecycleUplink(t, database, "member", "SHA256:member", false)
	expiry, err := database.AdminTokenExpiry(t.Context())
	if err != nil || expiry == nil || expiry.Warning != "" || expiry.ExpiringWithin30Days != 0 ||
		!expiry.EarliestExpiresAt.Equal(now.Add(90*24*time.Hour).Truncate(time.Nanosecond)) {
		t.Fatalf("90-day admin: %+v, %v", expiry, err)
	}
	// Non-admin tokens never count; an admin token 10 days out warns.
	if _, err := database.db.ExecContext(t.Context(), `UPDATE token_credentials SET expires_at=? WHERE id=?`, unixNano(now.Add(24*time.Hour)), scoped.TokenCredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(t.Context(), `UPDATE token_credentials SET expires_at=? WHERE id=?`, unixNano(now.Add(10*24*time.Hour)), admin.TokenCredentialID); err != nil {
		t.Fatal(err)
	}
	expiry, err = database.AdminTokenExpiry(t.Context())
	if err != nil || expiry == nil || expiry.ExpiringWithin30Days != 1 || expiry.Warning == "" ||
		!expiry.EarliestExpiresAt.Equal(now.Add(10*24*time.Hour)) {
		t.Fatalf("10-day admin: %+v, %v", expiry, err)
	}
	// An already expired token is no longer an active admin.
	if _, err := database.db.ExecContext(t.Context(), `UPDATE token_credentials SET expires_at=? WHERE id=?`, unixNano(now.Add(-time.Hour)), admin.TokenCredentialID); err != nil {
		t.Fatal(err)
	}
	if expiry, err := database.AdminTokenExpiry(t.Context()); err != nil || expiry != nil {
		t.Fatalf("expired admin: %+v, %v", expiry, err)
	}
}
