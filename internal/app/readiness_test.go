package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestReadinessSnapshotNeverProbesOnRequestAndFailsClosed(t *testing.T) {
	var probes atomic.Int32
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}},
		Configured: func(context.Context) error { return nil },
		Cluster:    func(context.Context) error { probes.Add(1); return nil },
		Audit:      func(context.Context) error { return nil },
		VCS:        func(context.Context, model.Upstream) error { return nil },
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
	if probes.Load() != 1 {
		t.Fatalf("readiness requests made %d probes", probes.Load())
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
	health.Cluster = func(context.Context) error { return errors.New("cluster down") }
	health.refreshReadiness(t.Context())
	if health.Ready(t.Context()) == nil {
		t.Fatal("failed refresh retained green sample")
	}
}
