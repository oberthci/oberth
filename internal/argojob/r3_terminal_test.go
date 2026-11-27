package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
	"io"
	"k8s.io/client-go/util/workqueue"
	"log/slog"
	"testing"
	"time"
)

func TestFinishedRetriesUntilAcknowledgedAfterOneMinute(t *testing.T) {
	t.Parallel()
	for _, replay := range []wf.WorkflowPhase{wf.WorkflowRunning, wf.WorkflowFailed} {
		t.Run(string(replay), func(t *testing.T) {
			c, f, _ := observationFixture(t)
			f.Object.Status.Phase = wf.WorkflowSucceeded
			f.Object.Status.Message = "first terminal detail"
			cache := &fake.Cache{WorkflowObject: f.Object}
			c.Cache = cache
			now := time.Unix(100, 0)
			recovery := now.Add(time.Minute)
			c.Now = func() time.Time { return now }
			c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			c.RateLimiter = &advancingBackoff{TypedRateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, 8*time.Second), now: &now}
			calls, accepted := 0, 0
			c.Reporter = &reportSpy{FinishedFunc: func(_ context.Context, _ string, o argojob.Observation) error {
				calls++
				if o.Status != stepmodel.RunPassed || o.Phase != stepmodel.NativeSucceeded || o.Detail != "first terminal detail" {
					t.Error("terminal fact changed", o)
				}
				if now.Before(recovery) {
					f.Object.Status.Phase = replay
					f.Object.Status.Message = "later detail"
					return errors.New("Finished temporarily refused")
				}
				accepted++
				return nil
			}}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_ = argojob.New(c).Run(ctx)
			if calls < 11 || accepted != 1 || c.Metrics != nil && c.Metrics.IncompleteEvidence.Load() != 0 {
				t.Fatal("unacknowledged terminal abandoned", calls, accepted, now)
			}
		})
	}
}
func TestEvidenceRefusalsResetAfterSuccessfulObservation(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	n := nativeNode("one", "", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowFailed
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	cache := &fake.Cache{WorkflowObject: f.Object}
	c.Cache = cache
	c.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Nanosecond, time.Nanosecond)
	owner := c.Runs.(*statefake.Runs)
	calls := 0
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	c.Progress = &progressProbe{Runs: owner, begin: func(ctx context.Context, a stateport.Action, in stateport.EvidenceStart) (int64, error) {
		calls++
		if calls == 3 {
			n.FailedPodRestarts = 1
			f.Object.Status.Nodes[n.ID] = n
			cache.Changed(b.Workflow.Name)
			return owner.BeginEvidence(ctx, a, in)
		}
		return 0, errors.New("evidence refused")
	}, advance: func(ctx context.Context, id string) error { err := owner.AdvanceNative(ctx, id); cancel(); return err }}
	_ = argojob.New(c).Run(ctx)
	if calls != 8 {
		t.Fatal("refusals were not reset", calls)
	}
}
