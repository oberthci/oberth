package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type lifecycleStub struct {
	statuses  []store.UplinkStatus
	mutations int
	reads     int
}

func (s *lifecycleStub) ListUplinkStatuses(context.Context) ([]store.UplinkStatus, error) {
	s.reads++
	return s.statuses, nil
}
func (s *lifecycleStub) RevokeUplink(context.Context, string, string, string, string) (model.Uplink, error) {
	s.mutations++
	return model.Uplink{}, nil
}

func TestCredentialLifecycleRequiresUnscopedAdmin(t *testing.T) {
	fixture := newScopeFixture(t)
	stub := &lifecycleStub{}
	fixture.service.uplinkLifecycle = stub
	fixture.service.declarationDismissal = fixture.store
}

func TestCredentialLifecycleMutationsRequireAuditGate(t *testing.T) {
	fixture := newScopeFixture(t)
	sentinel := errors.New("audit unavailable")
	fixture.service.mutationGate = func(context.Context) error { return sentinel }
	for _, tool := range []string{"uplink_revoke", "uplink_rotate", "access_dismiss"} {
		if _, err := fixture.call(unscopedActor, tool, map[string]any{}); !errors.Is(err, sentinel) {
			t.Fatalf("%s bypassed audit gate: %v", tool, err)
		}
	}
}

func TestUnscopedAdminCanDismissOrphanedDeclaration(t *testing.T) {
	fixture := newScopeFixture(t)
	fixture.service.declarationDismissal = fixture.store
	if err := fixture.store.RecordGrantDeclarations(context.Background(), "removed-repo", "old", []store.GrantDeclaration{{Step: "old", Path: "oberth/data/old"}}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"repo": "removed-repo", "step": "old", "secret": "oberth/data/old"}
	if _, err := fixture.call(scopedAdmin, "access_dismiss", args); !errors.Is(err, ErrForbidden) {
		t.Fatalf("scoped orphan dismiss=%v", err)
	}
	if _, err := fixture.call(unscopedActor, "access_dismiss", args); err != nil {
		t.Fatal(err)
	}
	pending, err := fixture.store.PendingGrantDeclarations(context.Background(), "removed-repo")
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%+v %v", pending, err)
	}
}

func TestUplinkListReportsPendingActivation(t *testing.T) {
	fixture := newScopeFixture(t)
	fixture.service.uplinkLifecycle = &lifecycleStub{statuses: []store.UplinkStatus{{Uplink: model.Uplink{Identity: "pending-worker"}, Pending: true}}}
	value, err := fixture.call(unscopedActor, "uplink_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil || !strings.Contains(string(encoded), `"pending":true`) || !strings.Contains(string(encoded), `"revoked_at":null`) {
		t.Fatalf("pending response=%s err=%v", encoded, err)
	}
}
