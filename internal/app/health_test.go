package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type fakeHealthStore struct {
	upstreams    []model.Upstream
	repositories []model.Repository
	err          error
}

func (store fakeHealthStore) ListUpstreams(context.Context) ([]model.Upstream, error) {
	return store.upstreams, store.err
}

func (store fakeHealthStore) ListRepositories(context.Context) ([]model.Repository, error) {
	return store.repositories, store.err
}

func TestHealthRequiresConfiguredUpstreamAndCluster(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{}, Configured: func(context.Context) error { return nil },
		Cluster: func(context.Context) error { return nil }, VCS: func(context.Context, model.Upstream) error { return nil },
		Audit: func(context.Context) error { return nil },
	}
	// Vault-before-init semantics: with zero upstreams the pod must stay alive
	// but not ready, even when every other dependency is green.
	if err := health.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "no upstream is configured") {
		t.Fatalf("fresh install readiness = %v, want not-ready until an upstream is registered", err)
	}
	health.Store = fakeHealthStore{err: errors.New("database locked")}
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed with an unavailable database")
	}
	health.Store = fakeHealthStore{upstreams: []model.Upstream{{ID: 1}}}
	health.Configured = func(context.Context) error { return errors.New("credentials missing") }
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed without durable upstream credentials")
	}
	health.Configured = func(context.Context) error { return nil }
	health.VCS = func(context.Context, model.Upstream) error { return errors.New("VCS down") }
	if err := health.Ready(context.Background()); err != nil {
		t.Fatalf("transient VCS outage removed cached service from readiness: %v", err)
	}
	health.VCS = func(context.Context, model.Upstream) error { return nil }
	health.Cluster = func(context.Context) error { return errors.New("cluster down") }
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed with an unavailable cluster")
	}
	health.Cluster = func(context.Context) error { return nil }
	health.Audit = func(context.Context) error { return errors.New("anchor stale") }
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed with stale audit anchoring")
	}
	health.Audit = func(context.Context) error { return nil }
	if err := health.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHealthSkipsSSHCheckForLocalOnlyUpstreams(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{upstreams: []model.Upstream{{ID: 1, Kind: "local"}}},
		Configured: func(context.Context) error { return errors.New("SSH identity missing") },
		Cluster:    func(context.Context) error { return nil },
		Audit:      func(context.Context) error { return nil },
		VCS:        func(context.Context, model.Upstream) error { return nil },
	}
	// A local-only upstream does not need SSH credentials.
	if err := health.Ready(context.Background()); err != nil {
		t.Fatalf("local-only readiness failed: %v", err)
	}
	// Adding a non-local upstream should require SSH credentials.
	health.Store = fakeHealthStore{upstreams: []model.Upstream{
		{ID: 1, Kind: "local"},
		{ID: 2, Kind: "ssh"},
	}}
	if err := health.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed without SSH credentials despite non-local upstream")
	}
}

func TestHealthStatusIsUsefulDuringOutage(t *testing.T) {
	t.Parallel()
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{ID: 1}}, repositories: []model.Repository{{ID: 1}, {ID: 2}}},
		Configured: func(context.Context) error { return nil },
		Cluster:    func(context.Context) error { return errors.New("cluster down") },
		Audit:      func(context.Context) error { return nil },
		VCS:        func(context.Context, model.Upstream) error { return nil },
	}
	value, err := health.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.Database != "ready" || status.VCS != "ready" || status.Cluster != "unavailable" || status.Audit != "ready" || status.Upstreams != 1 || status.Repositories != 2 {
		t.Fatalf("status = %+v", status)
	}
}
