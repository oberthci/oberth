package argojob

import (
	"context"
	"sync"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
)

type occupiedCollection struct {
	Workflows
	started chan struct{}
	once    sync.Once
}

func (c *occupiedCollection) Get(ctx context.Context, _ string, _ meta.GetOptions) (*wf.Workflow, error) {
	c.once.Do(func() { close(c.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

type liveProgressNotice struct {
	ProgressStore
	updated chan struct{}
	once    sync.Once
	status  stepmodel.StepStatus
}

func (p *liveProgressNotice) SaveProgress(ctx context.Context, id string, progress stateport.Progress) error {
	if err := p.ProgressStore.SaveProgress(ctx, id, progress); err != nil {
		return err
	}
	if len(progress.Steps) > 0 {
		p.status = progress.Steps[0].Status
	}
	p.once.Do(func() { close(p.updated) })
	return nil
}

func occupiedCollector(t *testing.T, logs LogStore, phase wf.WorkflowPhase) (*Executor, *tracking) {
	t.Helper()
	e, live, native, _ := reviewR2Executor(t, &reviewR2Reporter{})
	e.Untrack(live.binding.Workflow.Name)
	native.w.Status.Phase = phase
	if phase == wf.WorkflowRunning {
		n := native.w.Status.Nodes["n"]
		n.Phase = wf.NodeRunning
		native.w.Status.Nodes[n.ID] = n
	}
	blocked := &occupiedCollection{started: make(chan struct{})}
	e.collector.config.Workflows = blocked
	e.config.Logs, e.collector.config.Logs = logs, logs
	archive := live.binding
	archive.RunID, archive.Workflow.Name = "old-run", "old-workflow"
	e.collector.track(archive, live.plan, trackingSeed{completed: true, archiveOnly: true})
	if logs != nil {
		e.collector.offerCapture(e.collector.tracked[archive.Workflow.Name], captureJob{binding: archive, entries: []runlog.ContainerLog{{Step: "build", PodUID: "old-pod", Container: "main"}}})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("collection worker did not stop with its owner")
		}
	})
	waitForLiveWork(t, blocked.started, "collection did not occupy its worker")
	return e, live
}

func waitForLiveWork(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func TestSaturatedCollectionDoesNotDelayLiveStatus(t *testing.T) {
	e, live := occupiedCollector(t, nil, wf.WorkflowRunning)
	progress := &liveProgressNotice{ProgressStore: e.config.Progress, updated: make(chan struct{})}
	e.config.Progress = progress
	wall := clock.RealClock{}
	start := wall.Now()
	e.track(live.binding, live.plan, trackingSeed{})
	waitForLiveWork(t, progress.updated, "collection delayed the live status update beyond one second")
	if progress.status != stepmodel.StepRunning {
		t.Fatal("live status update did not carry the running step", progress.status)
	}
	if e.config.Metrics.IncompleteEvidence.Load() != 0 {
		t.Fatal("collection consumed a live evidence window")
	}
	t.Log("live progress while collection is occupied:", wall.Since(start))
}

type liveFinishNotice struct {
	Reporter
	finished chan struct{}
	once     sync.Once
	status   stepmodel.RunStatus
}

func (r *liveFinishNotice) Finished(ctx context.Context, id string, o Observation) error {
	if err := r.Reporter.Finished(ctx, id, o); err != nil {
		return err
	}
	r.status = o.Status
	r.once.Do(func() { close(r.finished) })
	return nil
}

func TestSaturatedCollectionDoesNotDelayLiveFinish(t *testing.T) {
	e, live := occupiedCollector(t, nil, wf.WorkflowSucceeded)
	reporter := &liveFinishNotice{Reporter: e.config.Reporter, finished: make(chan struct{})}
	e.config.Reporter = reporter
	e.track(live.binding, live.plan, trackingSeed{})
	waitForLiveWork(t, reporter.finished, "collection delayed the live terminal status beyond one second")
	if reporter.status != stepmodel.RunPassed {
		t.Fatal("live terminal status changed during collection", reporter.status)
	}
}

type occupiedArchiveCopy struct {
	LogStore
	old, live chan struct{}
	once      sync.Once
}

func (l *occupiedArchiveCopy) ReserveFinal(ctx context.Context, id string, _ runlog.ContainerLog) (*runlog.Reservation, error) {
	if id == "old-run" {
		l.once.Do(func() { close(l.old) })
	} else {
		close(l.live)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*occupiedArchiveCopy) Containers(context.Context, string) ([]runlog.ContainerLog, error) {
	return nil, nil
}
func (*occupiedArchiveCopy) Track(context.Context, string, []runlog.ContainerLog) error { return nil }
func (*occupiedArchiveCopy) KeepFollowing(string, []runlog.ContainerLog)                {}

func TestSaturatedCollectionCopiesLeaveLiveCaptureCapacity(t *testing.T) {
	logs := &occupiedArchiveCopy{old: make(chan struct{}), live: make(chan struct{})}
	e, live := occupiedCollector(t, logs, wf.WorkflowRunning)
	waitForLiveWork(t, logs.old, "archive copy did not occupy its worker")
	e.track(live.binding, live.plan, trackingSeed{})
	e.mu.Lock()
	current := e.tracked[live.binding.Workflow.Name]
	e.mu.Unlock()
	e.offerCapture(current, captureJob{binding: live.binding, entries: []runlog.ContainerLog{{Step: "build", PodUID: "live-pod", Container: "main"}}})
	waitForLiveWork(t, logs.live, "archive copy consumed the live capture workers")
}
