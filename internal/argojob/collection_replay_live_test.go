package argojob

import (
	"context"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

type collectionReplayReads struct {
	collectionRunReader
	calls int
}

func (r *collectionReplayReads) Run(context.Context, string) (stateport.Run, error) {
	r.calls++
	return stateport.Run{}, publicerr.New(publicerr.NotFound, "No run", "Select a run.", nil)
}

type liveReplayCache struct {
	collectionQueueBarrier
	workflow *wf.Workflow
}

func (c liveReplayCache) Workflow(name string) (*wf.Workflow, bool) {
	if name == c.workflow.Name {
		return c.workflow.DeepCopy(), true
	}
	return c.collectionQueueBarrier.Workflow(name)
}

func TestCollectionReplayLeavesTheLiveObserverInCharge(t *testing.T) {
	e, live, native, _ := reviewR2Executor(t, &reviewR2Reporter{})
	reads := &collectionReplayReads{}
	barrier := collectionQueueBarrier{done: make(chan struct{})}
	e.collector.config.Runs = reads
	e.collector.config.Cache = liveReplayCache{collectionQueueBarrier: barrier, workflow: native.w}
	e.collector.queue.Add(live.binding.Workflow.Name)
	e.collector.queue.Add("barrier")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); e.collector.runCollection(ctx) }()
	t.Cleanup(func() { cancel(); e.collector.queue.ShutDown(); <-done })
	waitForLiveWork(t, barrier.done, "collection replay did not finish its turn")
	if reads.calls != 0 {
		t.Fatal("collection replay took ownership from the live observer", reads.calls)
	}
}
