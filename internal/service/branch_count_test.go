package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
)

type countingRefs struct {
	mu     sync.Mutex
	counts map[string]int
	err    error
	calls  []string
}

func (refs *countingRefs) RefSHA(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}

func (refs *countingRefs) NonDefaultBranchCount(ctx context.Context, repo string) (int, error) {
	refs.mu.Lock()
	defer refs.mu.Unlock()
	refs.calls = append(refs.calls, repo)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if refs.err != nil {
		return 0, refs.err
	}
	return refs.counts[repo], nil
}

func (refs *countingRefs) callCount() int {
	refs.mu.Lock()
	defer refs.mu.Unlock()
	return len(refs.calls)
}

type plainRefs struct{}

func (plainRefs) RefSHA(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}

func TestRepoListingsCarryCachedNonDefaultBranchCount(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	refs := &countingRefs{counts: map[string]int{"codeberg/acme/oberth": 3}}
	service, err := NewAPI(APIConfig{Runs: fixture.store, Repositories: fixture.store, History: fixture.store, Refs: refs})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	actor := api.Actor{Identity: "reader"}
	value, err := service.CallTool(ctx, actor, "repo_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	rows := value.(api.RepoListResponse).Repositories.([]repoSummary)
	if len(rows) != 1 || rows[0].NonDefaultBranches == nil || *rows[0].NonDefaultBranches != 3 {
		t.Fatalf("repo_list rows = %+v", rows)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Repositories []map[string]json.RawMessage `json:"repositories"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || string(decoded.Repositories[0]["non_default_branches"]) != "3" {
		t.Fatalf("repo_list JSON = %s (%v)", raw, err)
	}
	// The dashboard and repeated reads reuse the cached figure: one ref walk.
	for range 3 {
		views, err := service.Repositories(ctx, actor)
		if err != nil {
			t.Fatal(err)
		}
		list := views.([]RepoView)
		if len(list) != 1 || list[0].NonDefaultBranches == nil || *list[0].NonDefaultBranches != 3 {
			t.Fatalf("dashboard views = %+v", list)
		}
	}
	if refs.callCount() != 1 {
		t.Fatalf("ref walks = %v, want one cached walk", refs.calls)
	}
}

func TestRepoListSkipsHiddenArchivedMirrors(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	refs := &countingRefs{counts: map[string]int{"codeberg/acme/oberth": 2}}
	service, err := NewAPI(APIConfig{Runs: fixture.store, Repositories: fixture.store, History: fixture.store, RepositoryArchiver: fixture.store, Refs: refs,
		ChangeRepositoryArchive: func(_ context.Context, _ string, _ bool, change func() error) error { return change() }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin := api.Actor{Identity: "admin", Admin: true}
	if _, err := service.CallTool(ctx, admin, "repo_archive", json.RawMessage(`{"repo":"oberth"}`)); err != nil {
		t.Fatal(err)
	}
	value, err := service.CallTool(ctx, admin, "repo_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if list := value.(api.RepoListResponse); list.ArchivedHidden != 1 || refs.callCount() != 0 {
		t.Fatalf("hidden archived repository was counted: %+v calls=%v", list, refs.calls)
	}
	value, err = service.CallTool(ctx, admin, "repo_list", json.RawMessage(`{"archived":"only"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows := value.(api.RepoListResponse).Repositories.([]repoSummary)
	if len(rows) != 1 || rows[0].NonDefaultBranches == nil || *rows[0].NonDefaultBranches != 2 {
		t.Fatalf("archived=only rows = %+v", rows)
	}
}

func TestRepoListLeavesCountUnknownWithoutCounter(t *testing.T) {
	t.Parallel()
	fixture := newControlFixture(t)
	service, err := NewAPI(APIConfig{Runs: fixture.store, Repositories: fixture.store, History: fixture.store, Refs: plainRefs{}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := service.CallTool(context.Background(), api.Actor{Identity: "reader"}, "repo_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	var decoded struct {
		Repositories []map[string]json.RawMessage `json:"repositories"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || string(decoded.Repositories[0]["non_default_branches"]) != "null" {
		t.Fatalf("unknown count JSON = %s (%v)", raw, err)
	}
}

func TestBranchCountCacheCachesFailuresAndExpires(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	cache := &branchCountCache{now: func() time.Time { return now }}
	refs := &countingRefs{counts: map[string]int{"a": 1}, err: errors.New("repository a is not cached")}
	ctx := context.Background()
	if got := cache.counts(ctx, []string{"a"}, refs.NonDefaultBranchCount); len(got) != 0 {
		t.Fatalf("failed count = %v, want unknown", got)
	}
	refs.err = nil
	if got := cache.counts(ctx, []string{"a"}, refs.NonDefaultBranchCount); len(got) != 0 || refs.callCount() != 1 {
		t.Fatalf("failure not cached: %v calls=%v", got, refs.calls)
	}
	now = now.Add(branchCountTTL)
	if got := cache.counts(ctx, []string{"a"}, refs.NonDefaultBranchCount); got["a"] != 1 || refs.callCount() != 2 {
		t.Fatalf("expired entry not reloaded: %v calls=%v", got, refs.calls)
	}
}

func TestBranchCountCacheDoesNotCacheSpentBudget(t *testing.T) {
	t.Parallel()
	cache := &branchCountCache{}
	refs := &countingRefs{counts: map[string]int{"a": 4, "b": 5}}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := cache.counts(canceled, []string{"a", "b"}, refs.NonDefaultBranchCount); len(got) != 0 || refs.callCount() != 0 {
		t.Fatalf("canceled listing = %v calls=%v", got, refs.calls)
	}
	got := cache.counts(context.Background(), []string{"a", "b"}, refs.NonDefaultBranchCount)
	if got["a"] != 4 || got["b"] != 5 {
		t.Fatalf("counts after canceled listing = %v", got)
	}
}
