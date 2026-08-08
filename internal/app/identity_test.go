package app

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type fakeUplinkCatalog struct {
	uplink model.Uplink
	err    error
}

func (catalog fakeUplinkCatalog) UplinkByFingerprint(context.Context, string) (model.Uplink, error) {
	return catalog.uplink, catalog.err
}

func TestSSHIdentitiesReturnsDurableActor(t *testing.T) {
	t.Parallel()
	identity, found, err := (SSHIdentities{Uplinks: fakeUplinkCatalog{uplink: model.Uplink{Identity: "agent@host"}}}).ResolveFingerprint("SHA256:key")
	if err != nil || !found || identity != "agent@host" {
		t.Fatalf("resolve = %q, %v, %v", identity, found, err)
	}
}

func TestSSHIdentitiesHidesUnknownKey(t *testing.T) {
	t.Parallel()
	identity, found, err := (SSHIdentities{Uplinks: fakeUplinkCatalog{err: store.ErrNotFound}}).ResolveFingerprint("SHA256:unknown")
	if err != nil || found || identity != "" {
		t.Fatalf("resolve = %q, %v, %v", identity, found, err)
	}
}

func TestSSHIdentitiesPropagatesStorageFailure(t *testing.T) {
	t.Parallel()
	failure := errors.New("database unavailable")
	_, found, err := (SSHIdentities{Uplinks: fakeUplinkCatalog{err: failure}}).ResolveFingerprint("SHA256:key")
	if found || !errors.Is(err, failure) {
		t.Fatalf("resolve = found %v, error %v", found, err)
	}
}
