package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/store"
)

func TestKubeVirtGrantToolRequiresAdminAndAuditsState(t *testing.T) {
	fixture := newControlFixture(t)
	control, err := NewAPI(APIConfig{Runs: fixture.store, Repositories: fixture.store, KubeVirtGrants: fixture.store})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw := json.RawMessage(`{"repo":"oberth"}`)
	lookup := func(wantState string) {
		t.Helper()
		value, err := control.CallTool(ctx, api.Actor{Identity: "auditor@host"}, "kubevirt_grant_get", raw)
		if err != nil {
			t.Fatal(err)
		}
		got := value.(map[string]any)
		if got["state"] != wantState || got["granted"] != (wantState == "granted") {
			t.Fatalf("grant lookup = %#v, want state %s", got, wantState)
		}
		grant, exists := got["grant"]
		if wantState == "never_granted" && exists {
			t.Fatalf("never-granted lookup has grant: %#v", got)
		}
		if wantState == "revoked" && (!exists || grant.(store.KubeVirtGrant).RevokedAt == nil) {
			t.Fatalf("revoked lookup loses audit record: %#v", got)
		}
	}
	lookup("never_granted")
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
	lookup("granted")
	if _, err := control.CallTool(ctx, admin, "kubevirt_revoke", raw); err != nil {
		t.Fatal(err)
	}
	active, err = fixture.store.ActiveKubeVirtGrant(ctx, fixture.repo.ID)
	if err != nil || active {
		t.Fatalf("revoked grant = %v, %v", active, err)
	}
	lookup("revoked")
}
