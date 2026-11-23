package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"testing"
	"time"
)

func TestDeletedWorkflowReportsOnceAndStopsTracking(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	f.Object = nil
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reports := 0
	c.Reporter = &reportSpy{FinishedFunc: func(_ context.Context, id string, o argojob.Observation) error {
		reports++
		if id != b.RunID || o.Status != stepmodel.RunFailed || o.Reason != "workflow was deleted" {
			t.Error(o)
		}
		cancel()
		return nil
	}}
	_ = argojob.New(c).Run(ctx)
	if reports != 1 || f.Gets != 1 {
		t.Fatal(reports, f.Gets)
	}
}
func TestStoreObservationRefusalsKeepLiveWorkflowTracked(t *testing.T) { checkLiveRefusal(t, "store") }
func TestRestartRestoresStoredFailureBeforeShutdown(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	s := c.Runs.(*statefake.Runs)
	n := nativeNode("one", "", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	run, err := s.Run(t.Context(), b.RunID)
	compositionOK(t, err)
	first := project(t, argojob.Projection{Workflow: f.Object, Plan: run.Admission.Plan, Pods: []*core.Pod{nativePod(f.Object, n, 7)}})
	compositionOK(t, s.SaveProgress(t.Context(), b.RunID, stateport.Progress{Steps: first.Steps}))
	sequence, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: b.RunID, Nodes: len(first.Nodes)})
	compositionOK(t, err)
	compositionOK(t, s.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: b.RunID, Sequence: sequence, Nodes: first.Nodes}))
	f.Object.Status.Phase = wf.WorkflowFailed
	f.Object.Spec.Shutdown = wf.ShutdownStrategyStop
	f.Object.Status.Nodes = nil
	c.Cache = &fake.Cache{WorkflowObject: f.Object}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reports := 0
	c.Reporter = &reportSpy{FinishedFunc: func(_ context.Context, _ string, o argojob.Observation) error {
		reports++
		if o.Status != stepmodel.RunCancelled || o.Steps[0].Status != stepmodel.StepFailed || o.Reason != "stopped" || o.Steps[0].DisplayReason != "exited with code 7" || o.Steps[0].FailureClass != stepmodel.FailureAuthor || o.Nodes[0].Exit == nil {
			t.Error("restart lost failure", o)
		}
		cancel()
		return nil
	}}
	_ = argojob.New(c).Run(ctx)
	if reports != 1 {
		t.Fatal("not recovered", reports)
	}
}
func TestTerminalPublicationDoesNotDiscardLatePodEvidence(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	s := c.Runs.(*statefake.Runs)
	n := nativeNode("one", "", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowFailed
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	cache := &fake.Cache{WorkflowObject: f.Object}
	c.Cache = cache
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reports, writes := 0, 0
	c.Reporter = &reportSpy{FinishedFunc: func(context.Context, string, argojob.Observation) error { reports++; return nil }}
	c.Progress = &progressProbe{Runs: s, complete: func(context.Context, stateport.Action, stateport.EvidenceCompletion) error {
		t.Error("incomplete evidence completed")
		return nil
	}, save: func(ctx context.Context, id string, progress stateport.Progress) error {
		writes++
		err := s.SaveProgress(ctx, id, progress)
		if writes == 1 {
			cache.PodObjects = []*core.Pod{nativePod(f.Object, n, 7)}
			cache.Changed(f.Object.Name)
		} else {
			if progress.Steps[0].DisplayReason != "exited with code 7" || len(progress.Steps[0].Logs) != 1 {
				t.Error(progress.Steps)
			}
			cancel()
		}
		return err
	}}
	_ = argojob.New(c).Run(ctx)
	if reports != 1 || writes != 2 {
		t.Fatal(reports, writes)
	}
}

type bindingRefusal struct{ owner *statefake.Runs }

func (b bindingRefusal) Obligations(ctx context.Context, id string) ([]stateport.Obligation, error) {
	return b.owner.Obligations(ctx, id)
}

func (b bindingRefusal) BindWorkflow(context.Context, stateport.Action, nativebinding.RunBinding) error {
	return errors.New("binding unavailable")
}
func TestFindBindsCrashBeforeUIDPersistenceWithoutPosting(t *testing.T) {
	t.Parallel()
	p, _, f := fixture(t)
	s := memoryAdmission(t, p)
	e := argojob.New(argojob.Config{Workflows: f, Runs: bindingRefusal{s}})
	if _, err := e.Create(t.Context(), p); err == nil {
		t.Fatal("binding refusal hidden")
	}
	r, err := s.Run(t.Context(), p.View().RunID)
	compositionOK(t, err)
	if r.Origin.WorkflowUID != "" {
		t.Fatal("fixture already bound")
	}
	b, found, err := argojob.New(argojob.Config{Workflows: f, Runs: s}).Find(t.Context(), r)
	compositionOK(t, err)
	if !found || f.Posts != 1 || b.Workflow.UID == "" {
		t.Fatal(found, f.Posts, b)
	}
	r, err = s.Run(t.Context(), r.ID)
	compositionOK(t, err)
	if r.Origin.WorkflowUID != b.Workflow.UID {
		t.Fatal("not persisted")
	}
}

func (b bindingRefusal) Run(ctx context.Context, id string) (stateport.Run, error) {
	return b.owner.Run(ctx, id)
}
func (b bindingRefusal) Runs(ctx context.Context, q stateport.RunFilter) ([]stateport.Run, error) {
	return b.owner.Runs(ctx, q)
}
func (b bindingRefusal) Steps(ctx context.Context, id string) ([]stepmodel.RunStep, error) {
	return b.owner.Steps(ctx, id)
}

func TestEvidenceRefusalsStopAfterFiveAttempts(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	n := nativeNode("one", "", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowFailed
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	c.Cache = &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{nativePod(f.Object, n, 7)}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	attempts, terminals := 0, 0
	c.Metrics = &argojob.Metrics{}
	cause := errors.New("evidence refused")
	c.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Nanosecond, time.Nanosecond)
	c.Progress = &progressProbe{Runs: c.Runs.(*statefake.Runs), advance: func(ctx context.Context, id string) error {
		err := c.Runs.(*statefake.Runs).AdvanceNative(ctx, id)
		cancel()
		return err
	}, begin: func(ctx context.Context, a stateport.Action, in stateport.EvidenceStart) (int64, error) {
		attempts++
		return 0, cause
	}}
	c.Reporter = &reportSpy{FinishedFunc: func(context.Context, string, argojob.Observation) error { terminals++; return nil }}
	_ = argojob.New(c).Run(ctx)
	if c.Metrics.IncompleteEvidence.Load() != 1 {
		t.Error("evidence failure metric missing")
	}
	if attempts != 5 || terminals != 1 {
		t.Fatal(attempts, terminals)
	}
}

type catalogProbe struct {
	bindingRefusal
	ready chan struct{}
}

func (c catalogProbe) Runs(ctx context.Context, q stateport.RunFilter) ([]stateport.Run, error) {
	rows, err := c.owner.Runs(ctx, q)
	if q.Status == stepmodel.RunRunning {
		close(c.ready)
	}
	return rows, err
}
func (c catalogProbe) BindWorkflow(ctx context.Context, a stateport.Action, b nativebinding.RunBinding) error {
	return c.owner.BindWorkflow(ctx, a, b)
}
func TestCreateTracksWhileObserverAlreadyRuns(t *testing.T) {
	t.Parallel()
	p, _, f := fixture(t)
	s := memoryAdmission(t, p)
	ready := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	writes := 0
	c := argojob.Config{Now: compositionClock, Runs: catalogProbe{bindingRefusal{s}, ready}, Workflows: f, Cache: &fake.Cache{}, Reporter: &reportSpy{}, Progress: &progressProbe{Runs: s, save: func(context.Context, string, stateport.Progress) error { writes++; cancel(); return nil }}}
	e := argojob.New(c)
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("recovery not ready")
	}
	_, err := e.Create(t.Context(), p)
	compositionOK(t, err)
	<-done
	if writes != 1 {
		t.Fatal("created workflow was not observed", writes)
	}
}

func TestTerminalWaitsForLateContainerStatusesBeforeUntracking(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	n := nativeNode("one", "", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowFailed
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	p := nativePod(f.Object, n, 7)
	p.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
	cache := &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{p}}
	c.Cache = cache
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reports, writes := 0, 0
	s := c.Runs.(*statefake.Runs)
	c.Reporter = &reportSpy{FinishedFunc: func(context.Context, string, argojob.Observation) error { reports++; return nil }}
	c.Progress = &progressProbe{Runs: s, save: func(ctx context.Context, id string, progress stateport.Progress) error {
		writes++
		err := s.SaveProgress(ctx, id, progress)
		if writes == 1 {
			p.Status.ContainerStatuses = []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 7}}}, {Name: "wait", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{}}}}
			cache.Changed(f.Object.Name)
		} else {
			cancel()
		}
		return err
	}}
	_ = argojob.New(c).Run(ctx)
	if reports != 1 || writes != 2 {
		t.Fatal("late statuses lost", reports, writes)
	}
}

type workflowReadProbe struct {
	*fake.Workflows
	after func()
}

func (p workflowReadProbe) Get(ctx context.Context, name string, o meta.GetOptions) (*wf.Workflow, error) {
	w, err := p.Workflows.Get(ctx, name, o)
	p.after()
	return w, err
}
func TestPublishedTerminalIsNotReplacedByLaterDeletion(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	n := nativeNode("one", "", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowFailed
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	cache := &fake.Cache{WorkflowObject: f.Object}
	c.Cache = cache
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reports := 0
	s := c.Runs.(*statefake.Runs)
	c.Workflows = workflowReadProbe{f, cancel}
	c.Reporter = &reportSpy{FinishedFunc: func(context.Context, string, argojob.Observation) error { reports++; return nil }}
	c.Progress = &progressProbe{Runs: s, save: func(ctx context.Context, id string, progress stateport.Progress) error {
		err := s.SaveProgress(ctx, id, progress)
		f.Object = nil
		cache.WorkflowObject = nil
		cache.Changed(b.Workflow.Name)
		return err
	}}
	_ = argojob.New(c).Run(ctx)
	if reports != 1 {
		t.Fatal("terminal replaced by deletion", reports)
	}
}
