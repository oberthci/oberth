package argojob_test

import (
	"context"
	"errors"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

// A reader told the run finished, by the event or a wait, reads final step
// states and counts: the observer saves them before it reports the verdict.
// Argo keeps the pending message PodInitializing on the succeeded node; the
// passed step carries no such detail.
func TestFinishedRunReportsFinalStepsAndCounts(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	s := c.Runs.(*statefake.Runs)
	f.Object.Status.Phase = wf.WorkflowSucceeded
	n := nativeNode("0", "(0)", wf.NodeSucceeded, 1)
	n.Name, n.Message = b.Workflow.Name+"(0)", "PodInitializing"
	retry := wf.NodeStatus{ID: "retry", Name: b.Workflow.Name, Type: wf.NodeTypeRetry, Children: []string{n.ID}}
	f.Object.Status.Nodes = wf.Nodes{n.ID: n, retry.ID: retry}
	c.Cache = &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{nativePod(f.Object, n, 0)}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var seen stateport.Run
	var steps []stepmodel.RunStep
	c.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, _ argojob.Observation) error {
		var runErr, stepsErr error
		seen, runErr = s.Run(ctx, id)
		steps, stepsErr = s.Steps(ctx, id)
		cancel()
		return errors.Join(runErr, stepsErr)
	}}
	_ = argojob.New(c).Run(ctx)
	if seen.Passed != 1 || seen.Pending+seen.Queued+seen.Running+seen.Unknown != 0 {
		t.Fatal("finished run counted unfinished steps", seen.LogicalStepCounts)
	}
	if len(steps) != 1 || steps[0].Status != stepmodel.StepPassed || steps[0].Detail != "" {
		t.Fatal("finished run reported a stale step", steps)
	}
	if seen.StartedAt == nil || !seen.StartedAt.Equal(n.StartedAt.Time) {
		t.Fatal("finished run has no start", seen.StartedAt)
	}
}
