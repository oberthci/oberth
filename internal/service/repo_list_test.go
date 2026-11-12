package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func TestRepoListReturnsOnlySlimScopedRecords(t *testing.T) {
	f := newScopeFixture(t)
	value, err := f.call(scopedActor, "repo_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	rows := value.(api.RepoListResponse).Repositories.([]repoSummary)
	canonical, err := f.store.QualifiedRepoName(t.Context(), f.visible.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != f.visible.ID || rows[0].Upstream != "github" || rows[0].GitPath != canonical || rows[0].LatestRun == nil {
		t.Fatalf("%+v", rows)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Repositories []map[string]json.RawMessage `json:"repositories"`
	}
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Repositories[0]) != 8 {
		t.Fatalf("not slim: %s", raw)
	}
	var latest map[string]json.RawMessage
	if err = json.Unmarshal(decoded.Repositories[0]["latest_run"], &latest); err != nil || len(latest) != 4 {
		t.Fatalf("latest: %s %v", raw, err)
	}
	if strings.Contains(string(raw), hiddenRepoName) || strings.Contains(string(raw), "LatestRuns") {
		t.Fatal(string(raw))
	}
	full, err := f.service.Repositories(t.Context(), scopedActor)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.([]RepoView)[0].LatestRuns) < 2 {
		t.Fatal("dashboard history lost")
	}
}

type failingRepoHistory struct {
	RunHistory
	cause error
}

func (history failingRepoHistory) ListLatestRunsPerRepo(context.Context, int) ([]model.Run, error) {
	return nil, history.cause
}

func TestRepoListReturnsHistoryFailure(t *testing.T) {
	f := newScopeFixture(t)
	failure := errors.New("history unavailable")
	f.service.history = failingRepoHistory{RunHistory: f.service.history, cause: failure}
	_, err := f.call(scopedActor, "repo_list", map[string]any{})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "list latest repository runs") {
		t.Fatal(err)
	}
}

// countingMirror delegates to a real Git cache and counts ref walks (#1508).
type countingMirror struct {
	*gitcache.Cache
	walks int
}

func (mirror *countingMirror) NonDefaultBranchCount(ctx context.Context, repo string) (int, error) {
	mirror.walks++
	return mirror.Cache.NonDefaultBranchCount(ctx, repo)
}

// TestRepoListCountsNonDefaultBranchesFromCachedMirror walks a real mirror
// holding the default branch, two branches and two server-owned refs: only the
// two branches count, in repo_list and /api/repos alike, from one cached walk.
func TestRepoListCountsNonDefaultBranchesFromCachedMirror(t *testing.T) {
	f := newPruneFixture(t)
	// newPruneFixture's upstream carries six non-default branches; keep two.
	for _, branch := range []string{"feature/c", "ci/keep", "master", "feature/unmerged"} {
		f.git(t, f.mirror, "update-ref", "-d", "refs/heads/"+branch)
	}
	f.git(t, f.mirror, "update-ref", "refs/oberth/x", f.initial)
	f.git(t, f.mirror, "update-ref", "refs/replace/y", f.initial)
	mirror := &countingMirror{Cache: f.cache}
	service, err := NewAPI(APIConfig{Runs: f.store, History: f.store, Repositories: f.store, Refs: mirror})
	if err != nil {
		t.Fatal(err)
	}
	actor := api.Actor{Identity: "reader"}
	value, err := service.CallTool(t.Context(), actor, "repo_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Repositories []map[string]json.RawMessage `json:"repositories"`
	}
	if err = json.Unmarshal(raw, &listed); err != nil || len(listed.Repositories) != 1 || string(listed.Repositories[0]["non_default_branches"]) != "2" {
		t.Fatalf("repo_list = %s (%v)", raw, err)
	}
	for range 3 {
		views, err := service.Repositories(t.Context(), actor)
		if err != nil {
			t.Fatal(err)
		}
		raw, err = json.Marshal(views)
		if err != nil {
			t.Fatal(err)
		}
		var rows []map[string]json.RawMessage
		if err = json.Unmarshal(raw, &rows); err != nil || len(rows) != 1 || string(rows[0]["non_default_branches"]) != "2" {
			t.Fatalf("/api/repos = %s (%v)", raw, err)
		}
	}
	if mirror.walks != 1 {
		t.Fatalf("ref walks = %d, want 1 cache refresh for four listings", mirror.walks)
	}
}
