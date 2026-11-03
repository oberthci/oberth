package service

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

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
func TestAccessRevokeDoesNotInventReadbackState(t *testing.T) {
	f := newControlFixture(t)
	s := f.api(t)
	s.secretAccess = f.store
	s.secretAccessReconciler = NewAccessReconciler(fake.NewClientset(), "oberth", f.store, log.New(io.Discard, "", 0))
	actor := api.Actor{Identity: "admin", Admin: true}
	if _, err := s.accessAllow(t.Context(), actor, "unregistered", "*", "oberth/data/test"); err != nil {
		t.Fatal(err)
	}
	s.secretAccess = missingRevocationReadback{f.store}
	value, err := s.accessRevoke(t.Context(), actor, "unregistered", "*", "oberth/data/test")
	if !errors.Is(err, ErrUnavailable) || value.RevokedAt != nil {
		t.Fatalf("invented revocation: %+v, %v", value, err)
	}
}
