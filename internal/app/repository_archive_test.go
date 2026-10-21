package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestArchivedRepositorySkipsSchedulesBeforeGit(t *testing.T) {
	t.Parallel()
	now := time.Now()
	repo := model.Repository{ID: 1, Name: "alpha", DefaultBranch: "main", ArchivedAt: &now}
	// A nil Git receiver would panic if a schedule read were attempted.
	schedules, enqueues, _, state := testSchedules(t, nil, []model.Repository{repo})
	schedules.Tick(context.Background(), now)
	if len(enqueues.requests) != 0 || len(state.events) != 0 {
		t.Fatal("archived schedule produced work")
	}
}

func TestArchivedUpstreamResolutionRefusesContactButAllowsCleanupIdentity(t *testing.T) {
	t.Parallel()
	now := time.Now()
	upstreams := Upstreams{Catalog: fakeUpstreamCatalog{repositories: map[string]model.Repository{"github/org/repo": {ID: 1, Name: "repo", UpstreamID: 1, ArchivedAt: &now}}, upstreams: []model.Upstream{{ID: 1, Name: "github", BaseURL: "ssh://git@example.invalid/org"}}}}
	if _, err := upstreams.Remote("github/org/repo"); !errors.Is(err, store.ErrRepositoryArchived) || !errors.Is(err, gitcache.ErrUpstreamRefused) {
		t.Fatalf("archived remote = %v", err)
	}
	if q, err := upstreams.QualifyInput("github/org/repo"); err != nil || q.Org != "org" {
		t.Fatalf("cleanup identity = %+v, %v", q, err)
	}
}

func TestHealthStopsProbesWhenLastRepositoryArchivedAndResumesOnRestore(t *testing.T) {
	t.Parallel()
	now := time.Now()
	calls := 0
	catalog := fakeHealthStore{upstreams: []model.Upstream{{ID: 1, Name: "github"}}, repositories: []model.Repository{{ID: 1, UpstreamID: 1}}}
	health := Health{Store: catalog, VCSCache: &VCSSnapshot{}, VCS: func(context.Context, model.Upstream) error { calls++; return nil }}
	if _, err := health.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("active probes = %d", calls)
	}
	catalog.repositories = []model.Repository{{ID: 1, UpstreamID: 1, ArchivedAt: &now}}
	health.Store = catalog
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if calls != 1 || status.Repositories != 1 || len(status.UpstreamInfo) != 1 || status.UpstreamInfo[0].Probe != "archived" {
		t.Fatalf("archive status = %+v; probes = %d", status, calls)
	}
	catalog.repositories = []model.Repository{{ID: 1, UpstreamID: 1}}
	health.Store = catalog
	if _, err := health.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("restore did not resume probe: %d", calls)
	}
}
