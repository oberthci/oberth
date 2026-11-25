package argojob

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type missingCollectionWorkflow struct {
	Workflows
	reads atomic.Int32
}

func (w *missingCollectionWorkflow) Get(_ context.Context, name string, _ meta.GetOptions) (*wf.Workflow, error) {
	w.reads.Add(1)
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, name)
}

type collectionQueueBarrier struct {
	Cache
	done chan struct{}
}

func (c collectionQueueBarrier) Workflow(name string) (*wf.Workflow, bool) {
	if name == "barrier" {
		close(c.done)
	}
	return nil, false
}

type collectionRunReader interface{ Runs }
type unusedCollectionOwner struct{ collectionRunReader }

func TestCollectionWorkerRetainsEightAttemptsPerWindow(t *testing.T) {
	e, live, _, _ := reviewR2Executor(t, &reviewR2Reporter{})
	collector := e.collector
	workflows := &missingCollectionWorkflow{}
	barrier := collectionQueueBarrier{done: make(chan struct{})}
	collector.config.Workflows, collector.config.Cache, collector.config.Runs = workflows, barrier, unusedCollectionOwner{}
	// The existing wall-clock test measures 5 QPS. This test isolates the
	// per-window budget on the actual collection worker with a fixed clock.
	collector.collectionReads = rate.NewLimiter(rate.Inf, 1)
	for i := range 16 {
		b := live.binding
		b.RunID, b.Workflow.Name = fmt.Sprint("old-run-", i), fmt.Sprint("old-workflow-", i)
		collector.track(b, live.plan, trackingSeed{completed: true, archiveOnly: true})
	}
	collector.queue.Add("barrier")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); collector.runCollection(ctx) }()
	t.Cleanup(func() { cancel(); collector.queue.ShutDown(); <-done })
	waitForLiveWork(t, barrier.done, "collection queue did not process its first window")
	if workflows.reads.Load() != 8 {
		t.Fatal("collection worker exceeded its eight-attempt window", workflows.reads.Load())
	}
}

func TestCollectionLiveArchiveFailureInvalidatesEvidenceOnce(t *testing.T) {
	e, live, _, now := reviewR2Executor(t, &reviewR2Reporter{})
	t.Cleanup(e.collector.queue.ShutDown)
	track := &tracking{binding: live.binding, archiveOnly: true, archiveLive: true, archiveEvidenceUntil: now.Add(-time.Second)}
	for range 3 {
		e.collector.liveArchivePending(t.Context(), track, &archiveCheckError{errArchivePending})
	}
	if e.config.Progress.(*reviewR2Progress).invalidated != 1 || e.config.Metrics.IncompleteEvidence.Load() != 1 {
		t.Fatal("live archive failure invalidated evidence more than once or lost its metric", e.config.Progress.(*reviewR2Progress).invalidated, e.config.Metrics.IncompleteEvidence.Load())
	}
}

func TestCollectionCleanupFailureKeepsCompleteEvidence(t *testing.T) {
	e, live, _, now := reviewR2Executor(t, &reviewR2Reporter{})
	t.Cleanup(e.collector.queue.ShutDown)
	track := &tracking{binding: live.binding, archiveOnly: true, archiveLive: true, archiveEvidenceUntil: now.Add(-time.Second)}
	e.collector.liveArchivePending(t.Context(), track, errors.New("cleanup patch refused"))
	if e.config.Progress.(*reviewR2Progress).invalidated != 0 || e.config.Metrics.IncompleteEvidence.Load() != 0 {
		t.Fatal("collection cleanup failure invalidated complete evidence")
	}
}

func TestCollectionArchiveFailureAfterTransientAttemptsInvalidatesEvidenceOnce(t *testing.T) {
	e, live, _, now := reviewR2Executor(t, &reviewR2Reporter{})
	t.Cleanup(e.collector.queue.ShutDown)
	track := &tracking{binding: live.binding, archiveOnly: true, archiveLive: true, archiveEvidenceUntil: now.Add(time.Minute)}
	for range observationAttempts {
		e.collector.liveArchivePending(t.Context(), track, &archiveCheckError{errArchivePending})
	}
	if track.archiveLive || e.config.Progress.(*reviewR2Progress).invalidated != 0 {
		t.Fatal("transient archive attempts exceeded their bound or invalidated early")
	}
	*now = now.Add(2 * time.Minute)
	for range 2 {
		e.collector.liveArchivePending(t.Context(), track, &archiveCheckError{errArchivePending})
	}
	if e.config.Progress.(*reviewR2Progress).invalidated != 1 || e.config.Metrics.IncompleteEvidence.Load() != 1 {
		t.Fatal("archive backoff lost the live evidence deadline", e.config.Progress.(*reviewR2Progress).invalidated, e.config.Metrics.IncompleteEvidence.Load())
	}
}
