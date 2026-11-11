package store

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestClientPinCannotBindAfterCredentialReplacement(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := t.Context()
	old, err := s.RegisterCertificateUplink(ctx, "operator", model.UplinkSpec{Identity: "agent", Fingerprint: "SHA256:agent"})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("replacement credential"))
	credential, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: old.Identity, Digest: digest[:]})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.RegisterUplink(ctx, "operator", model.UplinkSpec{Identity: old.Identity, Fingerprint: old.Fingerprint, TokenCredentialID: credential.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	if next.ID != old.ID || next.TokenCredentialID == old.TokenCredentialID {
		t.Fatal("fixture did not replace the same uplink registration")
	}
	pin := strings.Repeat("f", 64)
	if err := s.BindClientPin(ctx, "operator", old.ID, old.TokenCredentialID, pin, now.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal("in-flight issuance survived credential replacement", err)
	}
	if _, err := s.UplinkByClientPin(ctx, pin); !errors.Is(err, ErrNotFound) {
		t.Fatal("rejected pin authenticates", err)
	}
	if err := s.BindClientPin(ctx, "operator", next.ID, next.TokenCredentialID, pin, now.Add(time.Hour)); err != nil {
		t.Fatal("replacement registration cannot enroll", err)
	}
}

func TestCertificateOnlyUplinkLifecycle(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := t.Context()
	u, err := s.RegisterCertificateUplink(ctx, "operator", model.UplinkSpec{Identity: "agent", Fingerprint: "SHA256:agent"})
	if err != nil {
		t.Fatal(err)
	}
	var digest []byte
	if err := s.db.QueryRowContext(ctx, `SELECT digest FROM token_credentials WHERE id=?`, u.TokenCredentialID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticatedUplinkByDigest(ctx, digest); !errors.Is(err, ErrNotFound) {
		t.Fatal("certificate lifecycle authenticates as bearer", err)
	}
	pin := strings.Repeat("a", 64)
	next := strings.Repeat("b", 64)
	for _, p := range []string{pin, next} {
		if err := s.BindClientPin(ctx, "operator", u.ID, u.TokenCredentialID, p, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got, err := s.UplinkByClientPin(ctx, p); err != nil || got.Identity != u.Identity {
			t.Fatal("pin binding", err)
		}
	}
	other, _ := lifecycleUplink(t, s, "other", "SHA256:other", false)
	if err := s.BindClientPin(ctx, "operator", other.ID, other.TokenCredentialID, pin, now.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatal("rebound pin", err)
	}
	if _, err := s.UplinkByClientPin(ctx, strings.Repeat("c", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown pin", err)
	}
	if _, err := s.RemoveUplink(ctx, "operator", u.Identity, u.Fingerprint); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{pin, next} {
		if _, err := s.UplinkByClientPin(ctx, p); !errors.Is(err, ErrNotFound) {
			t.Fatal("removed uplink", err)
		}
	}
	if err := s.BindClientPin(ctx, "operator", u.ID, u.TokenCredentialID, pin, now.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatal("issued after removal", err)
	}
	if err := s.BindClientPin(ctx, "operator", other.ID, other.TokenCredentialID, pin, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := s.UplinkByClientPin(ctx, pin); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired pin", err)
	}
}

func TestCertificateAdminRemovalGuardAndAudit(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := model.WithAuthMethod(t.Context(), "cert")
	admin, err := s.RegisterCertificateUplink(ctx, "operator", model.UplinkSpec{Identity: "admin", Fingerprint: "SHA256:admin-cert", Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindClientPin(ctx, "operator", admin.ID, admin.TokenCredentialID, strings.Repeat("d", 64), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeUplink(ctx, "operator", admin.Identity, admin.Fingerprint, ""); !errors.Is(err, ErrInvalidState) {
		t.Fatal("last certificate admin revoked", err)
	}
	next, err := s.RegisterCertificateUplink(ctx, "operator", model.UplinkSpec{Identity: "admin", Fingerprint: "SHA256:next-cert", Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeUplink(ctx, "operator", admin.Identity, admin.Fingerprint, next.Fingerprint); !errors.Is(err, ErrInvalid) {
		t.Fatal("replacement without a live pin accepted", err)
	}
	if err := s.BindClientPin(ctx, "operator", next.ID, next.TokenCredentialID, strings.Repeat("e", 64), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeUplink(ctx, "operator", admin.Identity, admin.Fingerprint, next.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UplinkByClientPin(ctx, strings.Repeat("d", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked certificate authenticated", err)
	}
	action, err := s.AppendAuditAction(ctx, model.AuditActionSpec{Actor: "operator", Action: "test", ResourceType: "uplink", Details: `{"sequence":9007199254740993}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(action.Details, `"auth_method":"cert"`) || !strings.Contains(action.Details, "9007199254740993") {
		t.Fatal("audit authentication metadata lost or changed numeric precision")
	}
}
