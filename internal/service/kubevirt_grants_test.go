package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestKubeVirtGrantToolRequiresAdminAndAuditsState(t *testing.T) {
	fixture := newControlFixture(t)
	control, err := NewAPI(APIConfig{Runs: fixture.store, Repositories: fixture.store, KubeVirtGrants: fixture.store})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw := json.RawMessage(`{"repo":"oberth"}`)
	nonAdmin := api.Actor{Identity: "agent@host"}
	if _, err := control.CallTool(ctx, nonAdmin, "kubevirt_grant", raw); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nonadmin grant = %v", err)
	}
	active, err := fixture.store.ActiveKubeVirtGrant(ctx, fixture.repo.ID)
	if err != nil || active {
		t.Fatalf("nonadmin altered grant = %v, %v", active, err)
	}
	admin := api.Actor{Identity: "admin@host", Admin: true}
	if _, err := control.CallTool(ctx, admin, "kubevirt_grant", raw); err != nil {
		t.Fatal(err)
	}
	active, err = fixture.store.ActiveKubeVirtGrant(ctx, fixture.repo.ID)
	if err != nil || !active {
		t.Fatalf("admin grant = %v, %v", active, err)
	}
	if _, err := control.CallTool(ctx, admin, "kubevirt_revoke", raw); err != nil {
		t.Fatal(err)
	}
	active, err = fixture.store.ActiveKubeVirtGrant(ctx, fixture.repo.ID)
	if err != nil || active {
		t.Fatalf("revoked grant = %v, %v", active, err)
	}
}
