package store

import (
	"errors"
	"testing"
	"time"
)

func TestRemoveUplinkPreservesLastActiveUnscopedAdmin(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	admin, digest := lifecycleUplink(t, s, "operator", "SHA256:admin", true)
	if _, err := s.RemoveUplink(t.Context(), "operator", admin.Identity, admin.Fingerprint); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("last admin removal: %v, want ErrInvalidState", err)
	}
	if _, err := s.AuthenticatedUplinkByDigest(t.Context(), digest); err != nil {
		t.Fatalf("refusal revoked token: %v", err)
	}
	other, _ := lifecycleUplink(t, s, "operator", "SHA256:other", true)
	if _, err := s.RemoveUplink(t.Context(), "operator", admin.Identity, admin.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveUplink(t.Context(), "operator", other.Identity, other.Fingerprint); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("remaining admin removal: %v", err)
	}
}
