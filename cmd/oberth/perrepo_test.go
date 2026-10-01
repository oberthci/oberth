package main

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
)

type fakePerRepoStore struct {
	repos     []model.Repository
	upstreams []model.Upstream
	grants    map[int64]map[string]map[string]bool
}

func (s fakePerRepoStore) ListRepositories(_ context.Context) ([]model.Repository, error) {
	return append([]model.Repository(nil), s.repos...), nil
}

func (s fakePerRepoStore) ListUpstreams(_ context.Context) ([]model.Upstream, error) {
	return append([]model.Upstream(nil), s.upstreams...), nil
}

func (s fakePerRepoStore) ActiveSecretGrants(_ context.Context, repoID int64) (map[string]map[string]bool, error) {
	if s.grants == nil {
		return nil, nil
	}
	return s.grants[repoID], nil
}

func TestBuildPerRepoIdentitiesIncludesReposWithGrants(t *testing.T) {
	t.Parallel()
	store := fakePerRepoStore{
		repos: []model.Repository{
			{ID: 1, Name: "oberth", UpstreamID: 10},
			{ID: 2, Name: "no-grants-repo", UpstreamID: 10},
		},
		upstreams: []model.Upstream{
			{ID: 10, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/oberthci"},
		},
		grants: map[int64]map[string]map[string]bool{
			1: {"*": {"oberth/data/release/cosign-secret": true}},
			// repo 2 has no grants
		},
	}

	result, err := buildPerRepoIdentities(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}

	// The repo with grants should be present.
	key := "codeberg/oberthci/oberth"
	entry, ok := result[key]
	if !ok {
		t.Fatalf("expected entry for %q, got %v", key, result)
	}

	// The SA name should match the installer's deterministic derivation.
	wantSA := installer.PerRepoName("codeberg", "oberthci", "oberth")
	if entry.ServiceAccountName != wantSA {
		t.Fatalf("SA = %q, want %q", entry.ServiceAccountName, wantSA)
	}

	// The repo without grants should be absent.
	noGrantsKey := "codeberg/oberthci/no-grants-repo"
	if _, exists := result[noGrantsKey]; exists {
		t.Fatalf("repo without grants should not be in per-repo identities")
	}
}

func TestBuildPerRepoIdentitiesEmptyWhenNoGrants(t *testing.T) {
	t.Parallel()
	store := fakePerRepoStore{
		repos:     []model.Repository{{ID: 1, Name: "empty", UpstreamID: 10}},
		upstreams: []model.Upstream{{ID: 10, Name: "local", BaseURL: "/data/forge"}},
	}

	result, err := buildPerRepoIdentities(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty map, got %v", result)
	}
}

func TestBuildPerRepoIdentitiesMatchesArgojobKeyFormat(t *testing.T) {
	t.Parallel()
	store := fakePerRepoStore{
		repos:     []model.Repository{{ID: 1, Name: "terraform", UpstreamID: 10}},
		upstreams: []model.Upstream{{ID: 10, Name: "github", BaseURL: "ssh://git@github.com/skipops"}},
		grants: map[int64]map[string]map[string]bool{
			1: {"*": {"oberth/upstream/skipops/terraform/plan/gcp-sa": true}},
		},
	}

	result, err := buildPerRepoIdentities(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}

	// The key must match what argojob.canonicalRepoKey produces.
	key := "github/skipops/terraform"
	if _, ok := result[key]; !ok {
		t.Fatalf("expected key %q, got keys %v", key, keys(result))
	}
}

func keys(m map[string]argojob.PerRepoIdentityConfig) []string {
	var result []string
	for k := range m {
		result = append(result, k)
	}
	return result
}
