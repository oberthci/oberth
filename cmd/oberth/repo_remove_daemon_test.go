package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestRepoRemoveUsesDaemonCacheCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, path, "github", "ssh://git@github.com/org")
	if err := runRepoAdd(t.Context(), []string{"--database", path, "repo", "github"}, io.Discard, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	called := false
	err := runRepoRemove(t.Context(), []string{"--database", path, "repo"}, io.Discard, repoDependencies{mutationGate: allowTestMutation, removeRepository: func(ctx context.Context, database, root, input string) (store.RemovedRepository, error) {
		called = true
		if database != path || input != "repo" {
			t.Errorf("removal identity: %s %s", database, input)
		}
		return removeTestRepository(ctx, database, root, input)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("CLI bypassed live cache owner")
	}
}
func TestDaemonCacheCleanupUsesLiveLocksAndChecksIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, path, "github", "ssh://git@github.com/org")
	if err := runRepoAdd(t.Context(), []string{"--database", path, "repo", "github"}, io.Discard, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	database, err := storetest.OpenAdminClient(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	root := t.TempDir()
	cache, err := gitcache.New(gitcache.Config{Root: root, Upstream: func(string) (string, error) { return "unused", nil }})
	if err != nil {
		t.Fatal(err)
	}
	server := withRepositoryRemoval(newAdminGateServer(func(context.Context) error { return nil }, path, nil), func(context.Context) error { return nil }, path, database, cache)
	for _, test := range []struct {
		database, root, repo string
		status               int
	}{{"wrong", root, "repo", http.StatusConflict}, {path, "wrong", "repo", http.StatusConflict}, {path, root, "../escape", http.StatusConflict}, {path, root, "repo", http.StatusOK}} {
		request := httptest.NewRequest(http.MethodPost, "/v1/repository-remove?repo="+url.QueryEscape(test.repo), nil)
		request.Header.Set("X-Oberth-Admin-Database", test.database)
		request.Header.Set("X-Oberth-Admin-Cache-Root", test.root)
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s: %d %s", test.repo, response.Code, response.Body.String())
		}
	}
}
func TestRepoRemoveDefersCatalogMutationToLiveOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, path, "github", "ssh://git@github.com/org")
	if err := runRepoAdd(t.Context(), []string{"--database", path, "repo", "github"}, io.Discard, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	called := false
	err := runRepoRemove(t.Context(), []string{"--database", path, "repo"}, io.Discard, repoDependencies{mutationGate: allowTestMutation, removeRepository: func(ctx context.Context, database, root, input string) (store.RemovedRepository, error) {
		called = true
		s, err := storetest.OpenAdminClient(ctx, database, store.Options{})
		if err != nil {
			return store.RemovedRepository{}, err
		}
		defer s.Close()
		if _, err := s.RepositoryByName(ctx, "repo"); err != nil {
			t.Errorf("catalog was removed before live owner acquired its locks: %v", err)
		}
		return removeTestRepository(ctx, database, root, input)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("live owner was not called")
	}
}
