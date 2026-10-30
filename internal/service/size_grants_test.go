package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
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
	if _, err := control.CallTool(ctx, api.Actor{Identity: "agent@host"}, "size_revoke", json.RawMessage(`{"repo":"oberth"}`)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("nonadmin opt-out = %v", err)
	}
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
	if !MutatingTool("size_grant") || !MutatingTool("size_revoke") || MutatingTool("size_grant_get") {
		t.Fatal("size grant mutations are not behind the mutation gate")
	}
}

func TestSizeGrantToolRevokesNeverGrantedTagPriorityAndRefreshesWaiter(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	tag, err := f.scheduler.AdmitRelease(ctx, ReleaseRequest{EventID: "opt-out", Repository: f.repo, Tag: "v1", ObjectSHA: strings.Repeat("b", 40), CommitSHA: strings.Repeat("c", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	g := f.scheduler.admission
	r := g.reserveRun(tag.Run)
	defer r.cancel()
	g.mu.Lock()
	r.prepared, r.size = true, periapsis.M
	g.mu.Unlock()
	control, err := NewAPI(APIConfig{Runs: f.store, Repositories: f.store, SizeGrants: f.store, Enqueues: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	admin := api.Actor{Identity: "admin", Admin: true}
	for _, tc := range []struct {
		tool string
		raw  string
		want model.RunPriority
	}{
		{"size_revoke", `{"repo":"oberth"}`, model.PriorityNormal},
		{"size_grant", `{"repo":"oberth","max":"L"}`, model.PriorityHighest},
	} {
		if _, err := control.CallTool(ctx, admin, tc.tool, json.RawMessage(tc.raw)); err != nil {
			t.Fatal(err)
		}
		g.mu.Lock()
		got := r.effectivePriority(time.Now())
		g.mu.Unlock()
		if got != tc.want {
			t.Fatalf("%s did not refresh waiter: %s", tc.tool, got)
		}
	}
}
