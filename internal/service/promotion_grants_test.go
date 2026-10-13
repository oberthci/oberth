package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// stubPromotionGrantStore implements PromotionGrantStore for testing.
type stubPromotionGrantStore struct {
	grants []model.PromotionGrant
}

func (s *stubPromotionGrantStore) AppendPromotionGrant(_ context.Context, spec model.PromotionGrantSpec) (model.PromotionGrant, error) {
	grant := model.PromotionGrant{
		ID:                "grant-" + spec.SourceSHA[:12],
		RepoID:            spec.RepoID,
		SourceSHA:         spec.SourceSHA,
		TargetRef:         spec.TargetRef,
		Issuer:            spec.Issuer,
		IssuerFingerprint: spec.IssuerFingerprint,
		Note:              spec.Note,
		CreatedAt:         time.Now(),
		ExpiresAt:         spec.ExpiresAt,
	}
	s.grants = append(s.grants, grant)
	return grant, nil
}

func (s *stubPromotionGrantStore) RevokePromotionGrant(_ context.Context, id, actor string) (model.PromotionGrant, error) {
	for i := range s.grants {
		if s.grants[i].ID == id && s.grants[i].RevokedAt == nil {
			now := time.Now()
			s.grants[i].RevokedBy = actor
			s.grants[i].RevokedAt = &now
			return s.grants[i], nil
		}
	}
	return model.PromotionGrant{}, store.ErrNotFound
}

func (s *stubPromotionGrantStore) MatchingPromotionGrant(_ context.Context, repoID int64, sha, target string) (model.PromotionGrant, error) {
	now := time.Now()
	for _, grant := range s.grants {
		if grant.RepoID == repoID && grant.SourceSHA == sha && grant.TargetRef == target && grant.Active(now) {
			return grant, nil
		}
	}
	return model.PromotionGrant{}, store.ErrNotFound
}

func (s *stubPromotionGrantStore) ConsumePromotionGrant(_ context.Context, grantID, promotionID, _ string) error {
	for i := range s.grants {
		if s.grants[i].ID == grantID && s.grants[i].ConsumedAt == nil {
			now := time.Now()
			s.grants[i].ConsumedBy = promotionID
			s.grants[i].ConsumedAt = &now
			return nil
		}
	}
	return store.ErrInvalidState
}

func (s *stubPromotionGrantStore) ListPromotionGrants(_ context.Context, _ model.PromotionGrantListFilter) ([]model.PromotionGrant, error) {
	return s.grants, nil
}

func (s *stubPromotionGrantStore) PromotionGrant(_ context.Context, id string) (model.PromotionGrant, error) {
	for _, grant := range s.grants {
		if grant.ID == id {
			return grant, nil
		}
	}
	return model.PromotionGrant{}, store.ErrNotFound
}

func TestPromoteWithGrantNonAdmin(t *testing.T) {
	t.Parallel()
	const sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fixture := newControlFixture(t)
	grantStore := &stubPromotionGrantStore{}

	// Pre-seed a green run for the source SHA.
	seedGreenPromotionCandidate(t, fixture, sourceSHA)

	service := fixture.apiWithGrants(t, grantStore)

	nonAdmin := api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent", Admin: false}
	admin := api.Actor{Identity: "admin@host", Fingerprint: "SHA256:admin", Admin: true}

	// Non-admin without a grant: must be rejected with ErrForbidden.
	_, err := service.CallTool(context.Background(), nonAdmin, "promote",
		json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("promote without grant error = %v, want ErrForbidden", err)
	}

	// Admin creates a grant for the exact (repo, sha, target).
	grantResult, err := service.CallTool(context.Background(), admin, "promote_grant",
		json.RawMessage(`{"repo":"oberth","sha":"`+sourceSHA+`","branch":"main","expires":"1h"}`))
	if err != nil {
		t.Fatalf("promote_grant error = %v", err)
	}
	grantMap, ok := grantResult.(map[string]any)
	if !ok {
		t.Fatalf("promote_grant result type = %T, want map[string]any", grantResult)
	}
	if grantMap["status"] != "active" {
		t.Fatalf("promote_grant status = %v, want active", grantMap["status"])
	}

	// Non-admin with a valid grant: should pass authorization and proceed
	// to the promotion workflow (may fail on stub git, but not on authz).
	_, err = service.CallTool(context.Background(), nonAdmin, "promote",
		json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
	if errors.Is(err, ErrForbidden) {
		t.Fatalf("promote with valid grant should not return ErrForbidden, got: %v", err)
	}
	// The promotion was admitted; any error is from the stub git layer.
}

func TestPromoteGrantRequiresAdmin(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	grantStore := &stubPromotionGrantStore{}
	service := fixture.apiWithGrants(t, grantStore)

	nonAdmin := api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent", Admin: false}

	// Non-admin must not create grants.
	_, err := service.CallTool(context.Background(), nonAdmin, "promote_grant",
		json.RawMessage(`{"repo":"oberth","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","branch":"main","expires":"1h"}`))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("promote_grant with non-admin error = %v, want ErrForbidden", err)
	}

	// Non-admin must not revoke grants.
	_, err = service.CallTool(context.Background(), nonAdmin, "promote_grant_revoke",
		json.RawMessage(`{"id":"grant-1"}`))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("promote_grant_revoke with non-admin error = %v, want ErrForbidden", err)
	}
}

func TestPromoteGrantListIsReadOnly(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	grantStore := &stubPromotionGrantStore{}
	service := fixture.apiWithGrants(t, grantStore)

	nonAdmin := api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent", Admin: false}

	// Non-admin can list grants (read-only).
	result, err := service.CallTool(context.Background(), nonAdmin, "promote_grant_list",
		json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("promote_grant_list with non-admin error = %v, want success", err)
	}
	grants, ok := result.([]map[string]any)
	if !ok {
		t.Fatalf("promote_grant_list result type = %T, want []map[string]any", result)
	}
	if len(grants) != 0 {
		t.Fatalf("promote_grant_list result length = %d, want 0", len(grants))
	}
}

func TestPromoteGrantRequiresFullSHA(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	grantStore := &stubPromotionGrantStore{}
	service := fixture.apiWithGrants(t, grantStore)

	admin := api.Actor{Identity: "admin@host", Fingerprint: "SHA256:admin", Admin: true}

	// Short SHA must be refused.
	_, err := service.CallTool(context.Background(), admin, "promote_grant",
		json.RawMessage(`{"repo":"oberth","sha":"aabbcc","branch":"main","expires":"1h"}`))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("promote_grant with short SHA error = %v, want ErrInvalidInput", err)
	}
}

func TestPromoteGrantWrongTargetRefused(t *testing.T) {
	t.Parallel()
	const sourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fixture := newControlFixture(t)
	grantStore := &stubPromotionGrantStore{}
	seedGreenPromotionCandidate(t, fixture, sourceSHA)
	service := fixture.apiWithGrants(t, grantStore)

	admin := api.Actor{Identity: "admin@host", Fingerprint: "SHA256:admin", Admin: true}
	nonAdmin := api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent", Admin: false}

	// Grant for target "staging", but promote to "main".
	_, err := service.CallTool(context.Background(), admin, "promote_grant",
		json.RawMessage(`{"repo":"oberth","sha":"`+sourceSHA+`","branch":"staging","expires":"1h"}`))
	if err != nil {
		t.Fatalf("promote_grant error = %v", err)
	}

	_, err = service.CallTool(context.Background(), nonAdmin, "promote",
		json.RawMessage(`{"sha":"`+sourceSHA+`","branch":"main"}`))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("promote with wrong-target grant error = %v, want ErrForbidden", err)
	}
}

func TestParseExpiry(t *testing.T) {
	t.Parallel()
	maxLifetime := 24 * time.Hour

	// Duration.
	result, err := parseExpiry("1h", maxLifetime)
	if err != nil {
		t.Fatalf("parseExpiry(1h) error = %v", err)
	}
	if time.Until(result) < 59*time.Minute || time.Until(result) > 61*time.Minute {
		t.Fatalf("parseExpiry(1h) = %v, want ~1h from now", time.Until(result))
	}

	// Duration exceeding max.
	_, err = parseExpiry("48h", maxLifetime)
	if err == nil {
		t.Fatal("parseExpiry(48h) should fail when max is 24h")
	}

	// Empty defaults to max.
	result, err = parseExpiry("", maxLifetime)
	if err != nil {
		t.Fatalf("parseExpiry('') error = %v", err)
	}
	if time.Until(result) < 23*time.Hour || time.Until(result) > 25*time.Hour {
		t.Fatalf("parseExpiry('') = %v, want ~24h from now", time.Until(result))
	}

	// Negative duration.
	_, err = parseExpiry("-1h", maxLifetime)
	if err == nil {
		t.Fatal("parseExpiry(-1h) should fail")
	}
}

// apiWithGrants is a helper that creates an API with promotion grants enabled.
func (fixture *controlFixture) apiWithGrants(t *testing.T, grants PromotionGrantStore) *API {
	t.Helper()
	service, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
		Enqueues: fixture.scheduler, Git: fixture.git, Refs: fixture.refs,
		Logs: fixture.logs, Auditor: fixture.store,
		Signals: fixture.signals, MaximumWait: 50 * time.Millisecond,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "promotion-work"),
		PromotionGrants:        grants,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
