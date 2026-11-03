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

type failingRepoGrantLookup struct {
	KubeVirtGrantStore
	repoID int64
}

func (s failingRepoGrantLookup) KubeVirtGrant(ctx context.Context, repoID int64) (store.KubeVirtGrant, error) {
	if repoID == s.repoID {
		return store.KubeVirtGrant{}, errors.New("grant lookup failed")
	}
	return s.KubeVirtGrantStore.KubeVirtGrant(ctx, repoID)
}

func TestRepositoryGrantMetadataIsActiveAndBestEffort(t *testing.T) {
	fixture := newScopeFixture(t)
	ctx := context.Background()
	fixture.service.kubeVirtGrants = fixture.store
	for _, repo := range []model.Repository{fixture.visible, fixture.hidden} {
		if _, err := fixture.store.SetKubeVirtGrant(ctx, repo.ID, "admin", true, store.KubeVirtBudget{}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(wantVisible bool) {
		t.Helper()
		value, err := fixture.call(unscopedActor, "repo_list", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		views := value.(api.RepoListResponse).Repositories.([]RepoView)
		if len(views) != 2 {
			t.Fatalf("listing lost repositories: %+v", views)
		}
		for _, view := range views {
			want := view.ID == fixture.hidden.ID || wantVisible
			if (view.KubeVirtGrant != nil) != want {
				t.Fatalf("unexpected grant metadata: %+v", view)
			}
		}
	}
	check(true)
	if _, err := fixture.store.SetKubeVirtGrant(ctx, fixture.visible.ID, "admin", false, store.KubeVirtBudget{}); err != nil {
		t.Fatal(err)
	}
	check(false)
	fixture.service.kubeVirtGrants = failingRepoGrantLookup{KubeVirtGrantStore: fixture.store, repoID: fixture.visible.ID}
	check(false)
}
