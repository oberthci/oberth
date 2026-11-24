package argojob_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	core "k8s.io/api/core/v1"
)

type unavailableArchives struct {
	*runlog.Store
	ready   atomic.Bool
	calls   atomic.Int32
	advance func()
}

func (s *unavailableArchives) Archived(ctx context.Context, id string, entries []runlog.ContainerLog) (bool, error) {
	if !s.ready.Load() {
		s.calls.Add(1)
		s.advance()
		return false, errors.New("archive commit unavailable")
	}
	return s.Store.Archived(ctx, id, entries)
}

func TestCollectionKeepsSourcesPastEvidenceDeadlineUntilArchivesCommit(t *testing.T) {
	t.Parallel()
	db, f, b, pod := collectionFixture(t)
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	compositionOK(t, err)
	defer func() { _ = logs.Close() }()
	var offset atomic.Int64
	checked := make(chan struct{}, 1)
	archives := &unavailableArchives{Store: logs, advance: func() {
		if offset.Swap(120) == 0 {
			return
		}
		select {
		case checked <- struct{}{}:
		default:
		}
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	reports := 1
	var premature atomic.Bool
	native := collectionNative{Workflows: f, collected: func(*wf.Workflow) {
		if !archives.ready.Load() {
			premature.Store(true)
		}
		cancel()
	}}
	cache := &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}
	config := argojob.Config{Runs: db, Progress: db, Logs: archives, Workflows: native, Pods: &fake.Pods{Object: pod}, Cache: cache, Reporter: collectionReporter(t, db, f, &reports), Now: func() time.Time { return compositionClock().Add(time.Duration(offset.Load()) * time.Second) }}
	done := make(chan struct{})
	go func() { defer close(done); _ = argojob.New(config).Run(ctx) }()
	select {
	case <-checked:
	case <-ctx.Done():
		t.Error("archive durability was never checked")
	}
	archives.ready.Store(true)
	<-done
	if premature.Load() || archives.calls.Load() == 0 || slices.Contains(f.Object.Finalizers, nativebinding.RecordFinalizer) {
		t.Fatal("cleanup escaped the durable archive gate", premature.Load(), archives.calls.Load(), f.Object.Finalizers, ctx.Err())
	}
	assertTimedOutArchive(t, db, logs, b.RunID)
}

func assertTimedOutArchive(t *testing.T, db *store.Store, logs *runlog.Store, id string) {
	t.Helper()
	record, err := db.Run(t.Context(), id)
	compositionOK(t, err)
	body, _, err := logs.Read(t.Context(), id, runlog.ReadRequest{OwnOnly: true})
	if err != nil || record.Status != stepmodel.RunPassed || record.EvidenceComplete || string(body) != "container output\n" {
		t.Fatal("archive completion rewrote timed-out evidence or lost output", record, string(body), err)
	}
}

func TestFinishedStepArchivesWhileWorkflowStillRuns(t *testing.T) {
	t.Parallel()
	db, f, b, pod := collectionFixture(t)
	f.Object.Status.Phase = wf.WorkflowRunning
	f.Object.DeletionTimestamp = nil
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	compositionOK(t, err)
	defer func() { _ = logs.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	config := argojob.Config{Runs: db, Progress: db, Logs: logs, Workflows: f, Pods: &fake.Pods{Object: pod}, Cache: &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}, Now: compositionClock}
	done := make(chan struct{})
	go func() { defer close(done); _ = argojob.New(config).Run(ctx) }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		body, _, err := logs.Read(ctx, b.RunID, runlog.ReadRequest{OwnOnly: true})
		if err == nil && string(body) == "container output\n" {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			cancel()
			<-done
			t.Fatal("finished step was not archived before Workflow completion", err)
		}
	}
	cancel()
	<-done
	if f.Object.Status.Phase != wf.WorkflowRunning || f.Patches != 0 {
		t.Fatal("step archive required Workflow completion or permitted early cleanup")
	}
}
