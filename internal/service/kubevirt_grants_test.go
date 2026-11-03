package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	_, active, err := fixture.store.ActiveKubeVirtGrant(ctx, fixture.repo.ID)
	if err != nil || active {
		t.Fatalf("nonadmin altered grant = %v, %v", active, err)
	}
	admin := api.Actor{Identity: "admin@host", Admin: true}
	if _, err := control.CallTool(ctx, admin, "kubevirt_grant", raw); err != nil {
		t.Fatal(err)
	}
	budget, active, err := fixture.store.ActiveKubeVirtGrant(ctx, fixture.repo.ID)
	if err != nil || !active || budget != store.DefaultKubeVirtBudget() {
		t.Fatalf("admin grant = %+v, %v, %v", budget, active, err)
	}
	lookup("granted")
	for _, bad := range []struct{ args, want string }{
		{`{"repo":"oberth","max_vms":17}`, "max VMs must be 1..16"},
		{`{"repo":"oberth","max_memory":"lots"}`, "KubeVirt max memory"},
		{`{"repo":"oberth","max_cores":0,"max_memory":"512Mi"}`, "1Gi..256Gi"},
	} {
		if _, err := control.CallTool(ctx, admin, "kubevirt_grant", json.RawMessage(bad.args)); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), bad.want) {
			t.Fatalf("grant %s = %v, want invalid input containing %q", bad.args, err, bad.want)
		}
	}
	if _, err := control.CallTool(ctx, admin, "kubevirt_revoke", json.RawMessage(`{"repo":"oberth","max_vms":3}`)); err == nil {
		t.Fatal("revoke accepted a budget field")
	}
	value, err := control.CallTool(ctx, admin, "kubevirt_grant", json.RawMessage(`{"repo":"oberth","max_vms":3,"max_cores":12,"max_memory":"24Gi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := value.(map[string]any); got["max_memory"] != "24Gi" || got["grant"].(store.KubeVirtGrant).MaxVMs != 3 {
		t.Fatalf("budget grant = %#v", got)
	}
	budget, active, err = fixture.store.ActiveKubeVirtGrant(ctx, fixture.repo.ID)
	if err != nil || !active || budget != (store.KubeVirtBudget{MaxVMs: 3, MaxCores: 12, MaxMemoryBytes: 24 << 30}) {
		t.Fatalf("budget grant stored = %+v, %v, %v", budget, active, err)
	}
	if _, err := control.CallTool(ctx, admin, "kubevirt_revoke", raw); err != nil {
		t.Fatal(err)
	}
	_, active, err = fixture.store.ActiveKubeVirtGrant(ctx, fixture.repo.ID)
	if err != nil || active {
		t.Fatalf("revoked grant = %v, %v", active, err)
	}
	lookup("revoked")
}
