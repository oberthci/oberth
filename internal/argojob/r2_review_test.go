package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"

	"k8s.io/client-go/util/workqueue"
	"testing"
	"time"
)

func TestReviewR1PublishedTerminalNotReportedAgainOnEvidenceFailure(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	n := nativeNode("one", "", wf.NodeSucceeded, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowSucceeded
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	c.Cache = &fake.Cache{WorkflowObject: f.Object}
	c.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Nanosecond, time.Nanosecond)
	attempts := 0
	c.Progress = &progressProbe{Runs: c.Runs.(*statefake.Runs), begin: func(context.Context, stateport.Action, stateport.EvidenceStart) (int64, error) {
		attempts++
		return 0, errors.New("evidence write refused")
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var reports []argojob.Observation
	c.Reporter = &reportSpy{FinishedFunc: func(_ context.Context, _ string, o argojob.Observation) error {
		reports = append(reports, o)
		if o.Reason == "native observation failed" {
			cancel()
		}
		return nil
	}}
	_ = argojob.New(c).Run(ctx)
	if len(reports) != 1 {
		t.Fatalf("successful native terminal reported %d times after %d evidence attempts: first=%s last=%s/%s", len(reports), attempts, reports[0].Status, reports[len(reports)-1].Status, reports[len(reports)-1].Reason)
	}
}

func TestReviewR1EarlierRetryCannotReplaceLatestFailedAttempt(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	first, last := nativeNode("first", ".build(0)", wf.NodeFailed, 1), nativeNode("last", ".build(1)", wf.NodeFailed, 2)
	parent := wf.NodeStatus{ID: "retry", Type: wf.NodeTypeRetry, Name: w.Name + ".build", Children: []string{first.ID, last.ID}}
	w.Status.Nodes = wf.Nodes{first.ID: first, last.ID: last, parent.ID: parent}
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	before := project(t, argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, first, 7), nativePod(w, last, 9)}})
	first.Phase = wf.NodeRunning
	parent.Children = []string{first.ID}
	w.Status.Nodes = wf.Nodes{first.ID: first, parent.ID: parent}
	after := project(t, argojob.Projection{Workflow: w, Plan: plan, Previous: before.Steps, PreviousNodes: before.Nodes})
	if after.Steps[0].Status != stepmodel.StepFailed || after.Steps[0].AttemptCount != 2 {
		t.Fatalf("older retry snapshot regressed latest failure: before=%+v after=%+v raw=%+v", before.Steps[0], after.Steps[0], after.Nodes)
	}
}

func TestReviewR1ExpandedDynamicPlaceholderDoesNotReturn(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowFailed
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "fan", Dynamic: true}, {Path: "other"}}}
	other := nativeNode("other", ".other", wf.NodeFailed, 1)
	w.Status.Nodes = wf.Nodes{other.ID: other}
	before := project(t, argojob.Projection{Workflow: w, Plan: plan})
	n := nativeNode("one", ".fan(0:item)", wf.NodeFailed, 1)
	w.Status.Nodes[n.ID] = n
	after := project(t, argojob.Projection{Workflow: w, Plan: plan, Previous: before.Steps, PreviousNodes: before.Nodes})
	for _, s := range after.Steps {
		if s.Path == "fan" {
			t.Fatalf("expanded dynamic placeholder resurrected: %+v", after.Steps)
		}
	}
}

func TestReviewR1SameFailedAttemptRetainsLogSelector(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	n.Outputs = &wf.Outputs{ExitCode: new("7")}
	w.Status.Nodes[n.ID] = n
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	before := project(t, argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, n, 7)}})
	after := project(t, argojob.Projection{Workflow: w, Plan: plan, Previous: before.Steps, PreviousNodes: before.Nodes})
	if len(after.Steps[0].Logs) != 1 || after.Steps[0].Attempts[0].PodName == "" {
		t.Fatalf("same failed native attempt lost retained log identity: before=%+v after=%+v raw=%+v", before.Steps[0], after.Steps[0], after.Nodes)
	}
}

func TestReviewR1ZeroNodeObservationReachesEvidenceGuard(t *testing.T) {
	t.Parallel()
	c, f, _ := observationFixture(t)
	f.Object.Status.Phase = wf.WorkflowError
	c.Cache = &fake.Cache{WorkflowObject: f.Object}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	reports, starts := 0, 0
	c.Reporter = &reportSpy{FinishedFunc: func(context.Context, string, argojob.Observation) error { reports++; cancel(); return nil }}
	c.Progress = &progressProbe{Runs: c.Runs.(*statefake.Runs), save: func(context.Context, string, stateport.Progress) error { return nil }, begin: func(context.Context, stateport.Action, stateport.EvidenceStart) (int64, error) {
		starts++
		return 0, nil
	}}
	_ = argojob.New(c).Run(ctx)
	if reports != 1 || starts != 0 {
		t.Fatalf("terminal reports=%d zero-node evidence starts=%d", reports, starts)
	}
}
