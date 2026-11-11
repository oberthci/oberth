package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// The metrics-only scope (#1534) is reserved, alone, and never admin.
func TestMetricsScopeIsReservedAloneAndNeverAdmin(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := testStore(t, &now)
	resolved, err := s.ResolveUplinkScopes(ctx, []string{"metrics"})
	if err != nil || !model.MetricsOnly(resolved) {
		t.Fatalf("resolve metrics: %v %v", resolved, err)
	}
	if _, err := s.ResolveUplinkScopes(ctx, []string{"metrics", "github"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("combined metrics scope: %v", err)
	}
	scraper, digest := lifecycleUplink(t, s, "metrics@prometheus", "SHA256:metrics", false)
	if _, err := s.SetUplinkScope(ctx, "operator", scraper.Identity, scraper.Fingerprint, []string{"metrics"}); err != nil {
		t.Fatal(err)
	}
	authenticated, err := s.AuthenticatedUplinkByDigest(ctx, digest)
	if err != nil || !model.MetricsOnly(authenticated.Scope) || authenticated.Admin {
		t.Fatalf("metrics uplink authenticates as %+v %v", authenticated.Uplink, err)
	}
	admin, _ := lifecycleUplink(t, s, "admin@ops", "SHA256:admin", true)
	if _, err := s.SetUplinkScope(ctx, "operator", admin.Identity, admin.Fingerprint, []string{"metrics"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("admin narrowed to metrics: %v", err)
	}
	credential, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "metrics-admin", Digest: digestOf("metrics-admin")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterUplink(ctx, "operator", model.UplinkSpec{Identity: "x@y", Fingerprint: "SHA256:x", TokenCredentialID: credential.ID, Admin: true, Scope: []string{"metrics"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("admin metrics uplink registered: %v", err)
	}
	if model.ScopeCovers([]string{"metrics"}, "metrics") {
		t.Fatal("metrics scope covers an upstream")
	}
}

func digestOf(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}
