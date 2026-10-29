package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type readinessCountingStore struct {
	fakeHealthStore
	reads atomic.Int32
}

func (store *readinessCountingStore) ListUpstreams(ctx context.Context) ([]model.Upstream, error) {
	store.reads.Add(1)
	return store.fakeHealthStore.ListUpstreams(ctx)
}

func TestReadinessSnapshotNeverProbesOnRequestAndFailsClosed(t *testing.T) {
	database := &readinessCountingStore{fakeHealthStore: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}}
	health := Health{
		Store:      database,
		Configured: func(context.Context) error { return nil },
		Cluster:    func(context.Context) error { t.Fatal("readiness called Kubernetes"); return nil },
		Audit:      func(context.Context) error { t.Fatal("readiness called audit"); return nil },
		VCS:        func(context.Context, model.Upstream) error { t.Fatal("readiness called upstream"); return nil },
		Readiness:  &ReadinessSnapshot{},
	}
	if health.Ready(t.Context()) == nil {
		t.Fatal("ready without sample")
	}
	health.refreshReadiness(t.Context())
	for range 100 {
		if err := health.Ready(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if database.reads.Load() != 1 {
		t.Fatalf("readiness requests made %d database reads", database.reads.Load())
	}
	health.ReadyGate = func(context.Context) error { return errors.New("recovering") }
	if health.Ready(t.Context()) == nil {
		t.Fatal("startup gate bypassed")
	}
	health.ReadyGate = nil
	health.Readiness.checked = time.Now().Add(-time.Minute)
	if health.Ready(t.Context()) == nil {
		t.Fatal("stale green sample accepted")
	}
	database.err = errors.New("database unavailable")
	health.refreshReadiness(t.Context())
	if health.Ready(t.Context()) == nil {
		t.Fatal("failed refresh retained green sample")
	}
	database.err = nil
	health.refreshReadiness(t.Context())
	if err := health.Ready(t.Context()); err != nil {
		t.Fatalf("database recovered but readiness did not: %v", err)
	}
}

func TestReadinessDoesNotRequireDiagnosticProbes(t *testing.T) {
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{Kind: "ssh"}}},
		Configured: func(context.Context) error { return nil }, Readiness: &ReadinessSnapshot{},
	}
	health.refreshReadiness(t.Context())
	if err := health.Ready(t.Context()); err != nil {
		t.Fatalf("optional diagnostics changed healthy serving state: %v", err)
	}
}

func TestReadinessCanceledRefreshCannotPublishGreen(t *testing.T) {
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}},
		Configured: func(context.Context) error { return nil }, Readiness: &ReadinessSnapshot{},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	health.refreshReadiness(ctx)
	if err := health.Ready(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh = %v, want not ready", err)
	}
}
