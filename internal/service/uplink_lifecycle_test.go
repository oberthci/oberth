package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
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
	fixture.service.kubeVirtGrants = fixture.store
	ctx := context.Background()
	if err := fixture.store.RecordGrantDeclarations(ctx, "removed-repo", "old", []store.GrantDeclaration{{Step: "old", Path: "oberth/data/old"}}); err != nil {
		t.Fatal(err)
	}
	calls := map[string]map[string]any{
		"uplink_list":         {},
		"uplink_revoke":       {"identity": "target"},
		"uplink_rotate":       {"identity": "target", "fingerprint": "old", "replacement_fingerprint": "new"},
		"access_dismiss":      {"repo": "removed-repo", "step": "old", "secret": "oberth/data/old"},
		"kubevirt_grant_list": {},
	}
	for tool, args := range calls {
		for _, actor := range []api.Actor{scopedActor, scopedAdmin, {Identity: "ordinary"}} {
			t.Run(tool+"/"+actor.Identity+"/"+fmt.Sprint(actor.Admin), func(t *testing.T) {
				if _, err := fixture.call(actor, tool, args); !errors.Is(err, ErrForbidden) {
					t.Fatalf("expected forbidden: %v", err)
				}
			})
		}
	}
	if stub.mutations != 0 || stub.reads != 0 {
		t.Fatalf("unauthorized backend access: %+v", stub)
	}
	pending, err := fixture.store.PendingGrantDeclarations(ctx, "removed-repo")
	if err != nil || len(pending) != 1 {
		t.Fatalf("unauthorized dismissal: %+v %v", pending, err)
	}
}

func TestUnscopedAdminListsAllUplinksAndKubeVirtGrants(t *testing.T) {
	fixture := newScopeFixture(t)
	fixture.service.uplinkLifecycle = &lifecycleStub{statuses: []store.UplinkStatus{
		{Uplink: model.Uplink{Identity: "github-agent", Fingerprint: "SHA256:first", Scope: []string{"github"}}},
		{Uplink: model.Uplink{Identity: "codeberg-agent", Fingerprint: "SHA256:second", Scope: []string{"codeberg"}}},
	}}
	value, err := fixture.call(unscopedActor, "uplink_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if !strings.Contains(string(encoded), "SHA256:first") || !strings.Contains(string(encoded), "SHA256:second") {
		t.Fatalf("uplinks=%s", encoded)
	}
	fixture.service.kubeVirtGrants = fixture.store
	repos, err := fixture.store.ListRepositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if _, err := fixture.store.SetKubeVirtGrant(context.Background(), repo.ID, "admin", true, store.KubeVirtBudget{}); err != nil {
			t.Fatal(err)
		}
	}
	value, err = fixture.call(unscopedActor, "kubevirt_grant_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	grants := value.(map[string]any)["grants"].([]map[string]any)
	if len(grants) != len(repos) {
		t.Fatalf("grants=%+v repos=%+v", grants, repos)
	}
	encoded, _ = json.Marshal(value)
	if !strings.Contains(string(encoded), "codeberg") || !strings.Contains(string(encoded), "github") {
		t.Fatalf("grants=%s", encoded)
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
