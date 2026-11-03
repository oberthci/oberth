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
	"github.com/oberthci/oberth/internal/store"
)

func TestRepositoryArchiveToolsRequireUnscopedAdminAndKeepCatalog(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	var transitions []bool
	var cacheNames []string
	service, err := NewAPI(APIConfig{Runs: fixture.store, Repositories: fixture.store, History: fixture.store, RepositoryArchiver: fixture.store,
		ChangeRepositoryArchive: func(_ context.Context, name string, archived bool, change func() error) error {
			cacheNames = append(cacheNames, name)
			transitions = append(transitions, archived)
			return change()
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tool := range []string{"repo_archive", "repo_unarchive"} {
		for _, actor := range []api.Actor{{Identity: "reader"}, {Identity: "scoped", Admin: true, Scope: []string{"codeberg"}}} {
			if _, err := service.CallTool(ctx, actor, tool, json.RawMessage(`{"repo":"oberth"}`)); !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s with %+v = %v", tool, actor, err)
			}
		}
	}
	if len(transitions) != 0 {
		t.Fatal("unauthorized archive touched cache")
	}
	actor := api.Actor{Identity: "admin", Admin: true}
	value, err := service.CallTool(ctx, actor, "repo_archive", json.RawMessage(`{"repo":"oberth"}`))
	if err != nil {
		t.Fatal(err)
	}
	archived := value.(model.Repository)
	if archived.ArchivedAt == nil || archived.ID != fixture.repo.ID {
		t.Fatalf("archive = %+v", archived)
	}
	if len(cacheNames) != 1 || cacheNames[0] != "codeberg/acme/oberth" {
		t.Fatalf("cache inputs = %v", cacheNames)
	}
	value, err = service.CallTool(ctx, actor, "repo_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	views := value.(api.RepoListResponse).Repositories.([]RepoView)
	if len(views) != 1 || views[0].ArchivedAt == nil {
		t.Fatalf("catalog lost archive metadata: %+v", views)
	}
	// A caller retaining a pre-archive repository snapshot cannot enqueue.
	_, err = fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "late", Repository: fixture.repo, Branch: "main", SHA: strings.Repeat("a", 40), Actor: "agent"})
	if !errors.Is(err, store.ErrRepositoryArchived) {
		t.Fatalf("stale snapshot enqueue = %v", err)
	}
	if _, err = service.CallTool(ctx, actor, "repo_unarchive", json.RawMessage(`{"repo":"codeberg/acme/oberth"}`)); err != nil {
		t.Fatal(err)
	}
	if len(transitions) != 2 || !transitions[0] || transitions[1] {
		t.Fatalf("transitions = %v", transitions)
	}
}

func TestArchivedPushIsRefusedBeforeGit(t *testing.T) {
	t.Parallel()
	now := time.Now()
	git := &stubPushGit{}
	ingestor, err := NewPushIngestor(PushConfig{Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", ArchivedAt: &now}}, Discoverer: stubRepositoryDiscoverer{}, Git: git, Receives: &stubReceiveRecorder{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ingestor.Branch(context.Background(), BranchPush{EventID: "push", Repository: "repo", Branch: "main", Actor: "agent", NewOID: strings.Repeat("a", 40)})
	if !errors.Is(err, store.ErrRepositoryArchived) || git.ensureCalls != 0 {
		t.Fatalf("archived push = %v, Git calls = %d", err, git.ensureCalls)
	}
	_, err = ingestor.Tag(context.Background(), TagPush{EventID: "tag", Repository: "repo", Tag: "v1", Actor: "agent", NewOID: strings.Repeat("a", 40)})
	if !errors.Is(err, store.ErrRepositoryArchived) || git.ensureCalls != 0 {
		t.Fatalf("archived tag = %v, Git calls = %d", err, git.ensureCalls)
	}
}

func TestArchivedRepositoryStatusReadsHistoryWithoutGitCache(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	ctx := context.Background()
	run, err := fixture.store.EnqueueRun(ctx, model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "agent", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed, Phase: "passed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SetRepositoryArchived(ctx, "admin", fixture.repo.Name, true); err != nil {
		t.Fatal(err)
	}
	// An empty resolver simulates the removed cache. Active branch status would
	// refuse it; archived branch status must retain access to historical runs.
	service, err := NewAPI(APIConfig{Runs: fixture.store, History: fixture.store, Repositories: fixture.store, Refs: stubRefResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := service.CallTool(ctx, api.Actor{Identity: "reader"}, "status", json.RawMessage(`{"repo":"oberth","ref":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	if status := value.(StatusResponse); status.RunID != run.ID || status.Repository.ArchivedAt == nil {
		t.Fatalf("archive history = %+v", status)
	}
}

func TestArchivedRepositoryRefusesNewGrants(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	ctx := context.Background()
	service, err := NewAPI(APIConfig{Runs: fixture.store, Repositories: fixture.store, History: fixture.store,
		KubeVirtGrants: fixture.store, SizeGrants: fixture.store, PromotionGrants: fixture.store, SecretAccess: fixture.store})
	if err != nil {
		t.Fatal(err)
	}
	admin := api.Actor{Identity: "admin@host", Fingerprint: "SHA256:admin", Admin: true}
	// Grants issued while active stay revocable after archive.
	if _, err := service.CallTool(ctx, admin, "kubevirt_grant", json.RawMessage(`{"repo":"oberth"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CallTool(ctx, admin, "size_grant", json.RawMessage(`{"repo":"oberth","max":"XL"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SetRepositoryArchived(ctx, "admin", fixture.repo.Name, true); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	grants := []struct{ tool, args string }{
		{"kubevirt_grant", `{"repo":"oberth"}`},
		{"size_grant", `{"repo":"oberth","max":"L"}`},
		{"promote_grant", `{"repo":"oberth","sha":"` + sha + `","branch":"main","expires":"1h"}`},
		{"access_allow", `{"repo":"oberth","step":"publish","secret":"registry"}`},
	}
	for _, grant := range grants {
		_, err := service.CallTool(ctx, admin, grant.tool, json.RawMessage(grant.args))
		if !errors.Is(err, store.ErrRepositoryArchived) || !errors.Is(err, store.ErrInvalidState) || !strings.Contains(err.Error(), grant.tool) {
			t.Fatalf("%s on archived repository = %v", grant.tool, err)
		}
	}
	if _, err := fixture.store.MatchingPromotionGrant(ctx, fixture.repo.ID, sha, "main"); err == nil {
		t.Fatal("refused promote_grant was recorded")
	}
	qualified, err := fixture.store.QualifiedRepoName(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	accessGrants, err := fixture.store.SecretAccessList(ctx, qualified, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(accessGrants) != 0 {
		t.Fatalf("refused access_allow was recorded: %+v", accessGrants)
	}
	value, err := service.CallTool(ctx, admin, "size_grant_get", json.RawMessage(`{"repo":"oberth"}`))
	if err != nil || value.(map[string]any)["state"] != "granted" {
		t.Fatalf("refused size_grant changed the existing grant: %v %v", value, err)
	}
	for _, revoke := range []string{"kubevirt_revoke", "size_revoke"} {
		if _, err := service.CallTool(ctx, admin, revoke, json.RawMessage(`{"repo":"oberth"}`)); err != nil {
			t.Fatalf("%s on archived repository = %v", revoke, err)
		}
	}
	if _, err := fixture.store.SetRepositoryArchived(ctx, "admin", fixture.repo.Name, false); err != nil {
		t.Fatal(err)
	}
	for _, grant := range grants {
		if _, err := service.CallTool(ctx, admin, grant.tool, json.RawMessage(grant.args)); err != nil {
			t.Fatalf("%s after unarchive = %v", grant.tool, err)
		}
	}
}
