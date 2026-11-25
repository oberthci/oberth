package argojob_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/runlog"
	core "k8s.io/api/core/v1"
)

type delayedCompleteArchive struct {
	*runlog.Store
	offset *atomic.Int64
}

func (s delayedCompleteArchive) Archived(ctx context.Context, id string, entries []runlog.ContainerLog) (bool, error) {
	// Model a healthy completed copy whose collection turn arrives after the
	// evidence deadline. Queue delay grants no reason to invalidate evidence.
	s.offset.Store(120)
	return s.Store.Archived(ctx, id, entries)
}

func TestCollectionDelayKeepsCompleteLiveEvidence(t *testing.T) {
	t.Parallel()
	db, f, b, pod := collectionFixture(t)
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	compositionOK(t, err)
	defer func() { _ = logs.Close() }()
	var offset atomic.Int64
	archives := delayedCompleteArchive{Store: logs, offset: &offset}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reports, collected := 1, false
	native := collectionNative{Workflows: f, collected: func(*wf.Workflow) {
		collected = true
		assertCollectedHistory(t, db, logs, b.RunID)
		cancel()
	}}
	config := argojob.Config{Runs: db, Progress: db, Logs: archives, Workflows: native, Pods: &fake.Pods{Object: pod}, Cache: &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}, Reporter: collectionReporter(t, db, f, &reports), Now: func() time.Time { return compositionClock().Add(time.Duration(offset.Load()) * time.Second) }}
	_ = argojob.New(config).Run(ctx)
	if !collected || offset.Load() != 120 {
		t.Fatal("delayed healthy archive did not complete", collected, offset.Load(), ctx.Err())
	}
	assertCollectedHistory(t, db, logs, b.RunID)
}
