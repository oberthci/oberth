package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestSizeGrantToolRequiresAdminAndReportsTheMaximum(t *testing.T) {
	fixture := newControlFixture(t)
	control, err := NewAPI(APIConfig{Runs: fixture.store, Repositories: fixture.store, SizeGrants: fixture.store})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lookup := func(wantState string, wantMax periapsis.Size) {
		t.Helper()
		value, err := control.CallTool(ctx, api.Actor{Identity: "auditor@host"}, "size_grant_get", json.RawMessage(`{"repo":"oberth"}`))
		if err != nil {
			t.Fatal(err)
		}
		got := value.(map[string]any)
		if got["state"] != wantState || got["max_size"] != wantMax {
			t.Fatalf("size lookup = %#v, want %s/%s", got, wantState, wantMax)
		}
	}
	lookup("never_granted", periapsis.L)
	grantXL := json.RawMessage(`{"repo":"oberth","max":"XL"}`)
	if _, err := control.CallTool(ctx, api.Actor{Identity: "agent@host"}, "size_grant", grantXL); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nonadmin grant = %v", err)
	}
	admin := api.Actor{Identity: "admin@host", Admin: true}
	if _, err := control.CallTool(ctx, admin, "size_grant", json.RawMessage(`{"repo":"oberth","max":"M"}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("grant M = %v", err)
	}
	lookup("never_granted", periapsis.L)
	if _, err := control.CallTool(ctx, admin, "size_grant", grantXL); err != nil {
		t.Fatal(err)
	}
	lookup("granted", periapsis.XL)
	if _, err := control.CallTool(ctx, admin, "size_revoke", json.RawMessage(`{"repo":"oberth"}`)); err != nil {
		t.Fatal(err)
	}
	lookup("revoked", periapsis.L)
	if !mutatingTool("size_grant") || !mutatingTool("size_revoke") || mutatingTool("size_grant_get") {
		t.Fatal("size grant mutations are not behind the mutation gate")
	}
}
