package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type lifecycleStub struct {
	statuses  []store.UplinkStatus
	mutations int
}

func (s *lifecycleStub) ListUplinkStatuses(context.Context) ([]store.UplinkStatus, error) {
	return s.statuses, nil
}
func (s *lifecycleStub) RevokeUplink(context.Context, string, string, string, string) (model.Uplink, error) {
	s.mutations++
	return model.Uplink{}, nil
}

func TestUplinkLifecycleScopeAndAdmin(t *testing.T) {
	fixture := newScopeFixture(t)
	stub := &lifecycleStub{statuses: []store.UplinkStatus{
		{Uplink: model.Uplink{Identity: scopedActor.Identity, Fingerprint: scopedActor.Fingerprint, Scope: scopedActor.Scope}},
		{Uplink: model.Uplink{Identity: scopedActor.Identity, Fingerprint: "SHA256:hidden", Scope: []string{"codeberg"}}},
	}}
	fixture.service.uplinkLifecycle = stub
	for _, actor := range []api.Actor{scopedActor, scopedAdmin, unscopedActor} {
		value, err := fixture.call(actor, "uplink_list", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(value)
		if actor.Scoped() && strings.Contains(string(encoded), "hidden") {
			t.Fatal("scope leak", string(encoded))
		}
	}
	for _, tool := range []string{"uplink_revoke", "uplink_rotate"} {
		for _, actor := range []api.Actor{scopedActor, scopedAdmin, {Identity: "ordinary"}} {
			if _, err := fixture.call(actor, tool, map[string]any{}); !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s %v", tool, err)
			}
		}
	}
	if stub.mutations != 0 {
		t.Fatal("unauthorized mutation")
	}
}

func TestKubeVirtListAndDismissRespectScope(t *testing.T) {
	fixture := newScopeFixture(t)
	fixture.service.declarationDismissal = fixture.store
	fixture.service.kubeVirtGrants = fixture.store
	repos, err := fixture.store.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if _, err := fixture.store.SetKubeVirtGrant(context.Background(), repo.ID, "admin", true); err != nil {
			t.Fatal(err)
		}
	}
	value, err := fixture.call(scopedAdmin, "kubevirt_grant_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if strings.Contains(string(encoded), "codeberg") || !strings.Contains(string(encoded), "github") {
		t.Fatalf("grants=%s", encoded)
	}
	_, hidden := fixture.call(scopedAdmin, "access_dismiss", map[string]any{"repo": "codeberg/cloudtaser/" + fixture.hidden.Name, "step": "deploy", "secret": "oberth/data/old"})
	_, missing := fixture.call(scopedAdmin, "access_dismiss", map[string]any{"repo": "missing", "step": "deploy", "secret": "oberth/data/old"})
	if !errors.Is(hidden, store.ErrNotFound) || !errors.Is(missing, store.ErrNotFound) {
		t.Fatalf("hidden=%v missing=%v", hidden, missing)
	}
	if _, err := fixture.call(scopedActor, "kubevirt_grant_list", map[string]any{}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
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
	if _, err := fixture.call(scopedAdmin, "access_dismiss", args); !errors.Is(err, store.ErrNotFound) {
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
