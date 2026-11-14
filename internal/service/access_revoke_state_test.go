package service

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

type missingRevocationReadback struct{ SecretAccessStore }

func (s missingRevocationReadback) SecretAccessList(ctx context.Context, repo string, revoked bool) ([]store.SecretAccessGrant, error) {
	if revoked {
		return nil, nil
	}
	return s.SecretAccessStore.SecretAccessList(ctx, repo, revoked)
}
func TestAccessRevokeReturnsCommittedDatabaseState(t *testing.T) {
	f := newControlFixture(t)
	s := f.api(t)
	s.secretAccess = f.store
	actor := api.Actor{Identity: "admin", Admin: true}
	if _, err := s.accessAllow(t.Context(), actor, "upstream/org/unregistered", ".oberth/release.yaml", "*", "oberth/data/test"); err != nil {
		t.Fatal(err)
	}
	s.secretAccess = missingRevocationReadback{f.store}
	value, err := s.accessRevoke(t.Context(), actor, "upstream/org/unregistered", ".oberth/release.yaml", "*", "oberth/data/test")
	if err != nil || value.RevokedAt == nil {
		t.Fatalf("committed revocation missing: %+v, %v", value, err)
	}
}
