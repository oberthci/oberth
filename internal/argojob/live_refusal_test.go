package argojob_test

import (
	"bytes"
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"

	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func checkLiveRefusal(t *testing.T, stage string) {
	t.Helper()
	in := liveRefusalFixture(t)
	c, now := in.config, in.now
	recovered := now.Add(time.Minute)
	attempts, reports := 0, 0
	var logged bytes.Buffer
	c.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	cause := errors.New(stage + " refused observation")
	refuse := func() error {
		attempts++
		if now.Before(recovered) {
			return cause
		}
		in.finish()
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	reporter := &reportSpy{FinishedFunc: func(_ context.Context, id string, o argojob.Observation) error {
		reports++
		if id != in.id || o.Status != stepmodel.RunPassed || now.Before(recovered) {
			t.Error("live Workflow was failed by observer", o, attempts, now)
		}
		cancel()
		return nil
	}}
	c.Reporter = reporter

	owner := c.Runs.(*statefake.Runs)
	c.Progress = &progressProbe{Runs: owner, save: func(ctx context.Context, id string, progress stateport.Progress) error {
		if err := refuse(); err != nil {
			return err
		}
		return owner.SaveProgress(ctx, id, progress)
	}}
	_ = argojob.New(c).Run(ctx)
	if attempts < 11 || reports != 1 || now.Before(recovered) || !strings.Contains(logged.String(), in.id) || !strings.Contains(logged.String(), cause.Error()) {
		t.Fatal("live refusal did not recover", attempts, reports, now, logged.String())
	}
}

type liveRefusalInput struct {
	config argojob.Config
	now    *time.Time
	id     string
	finish func()
}

func liveRefusalFixture(t *testing.T) liveRefusalInput {
	t.Helper()
	c, f, b := observationFixture(t)
	now := time.Unix(100, 0)
	c.Now = func() time.Time { return now }
	c.RateLimiter = &advancingBackoff{TypedRateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, 8*time.Second), now: &now}
	f.Object.Status.Phase = wf.WorkflowRunning
	n := nativeNode("one", "", wf.NodeRunning, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	p := nativePod(f.Object, n, 0)
	p.Status.Phase = core.PodRunning
	p.Status.StartTime = &meta.Time{Time: now}
	p.Spec.ActiveDeadlineSeconds = new(int64(3600))
	p.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
	cache := &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{p}}
	c.Cache = cache
	return liveRefusalInput{config: c, now: &now, id: b.RunID, finish: func() {
		f.Object.Status.Phase = wf.WorkflowSucceeded
		n.Phase = wf.NodeSucceeded
		f.Object.Status.Nodes[n.ID] = n
		p.Status.ContainerStatuses[0].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{}}
		cache.Changed(b.Workflow.Name)
	}}
}
