package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"

	"testing"
	"time"
)

func TestReviewR2LateOlderAttemptKeepsNativeOrder(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	first, last := nativeNode("first", ".build(0)", wf.NodeFailed, 1), nativeNode("last", ".build(1)", wf.NodeSucceeded, 2)
	parent := wf.NodeStatus{ID: "retry", Name: w.Name + ".build", Type: wf.NodeTypeRetry, Children: []string{first.ID, last.ID}}
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	w.Status.Nodes = wf.Nodes{last.ID: last, parent.ID: parent}
	before := project(t, argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, last, 0)}})
	w.Status.Nodes[first.ID] = first
	after := project(t, argojob.Projection{Workflow: w, Plan: plan, Pods: []*core.Pod{nativePod(w, first, 7), nativePod(w, last, 0)}, Previous: before.Steps, PreviousNodes: before.Nodes})
	s := after.Steps[0]
	if s.Status != stepmodel.StepPassed || len(s.Attempts) != 2 || s.Attempts[1].NodeID != last.ID {
		t.Fatalf("earlier newly seen attempt replaces successful retry: %+v; nodes=%+v", s, after.Nodes)
	}
}

func TestReviewR2FailedAttemptDoesNotKeepRunningReason(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	n := nativeNode("one", ".build", wf.NodeRunning, 1)
	w.Status.Nodes[n.ID] = n
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}
	before := project(t, argojob.Projection{Workflow: w, Plan: plan})
	n.Phase = wf.NodeFailed
	w.Status.Nodes[n.ID] = n
	w.Status.Phase = wf.WorkflowFailed
	after := project(t, argojob.Projection{Workflow: w, Plan: plan, Previous: before.Steps, PreviousNodes: before.Nodes})
	if after.Reason == before.Steps[0].DisplayReason {
		t.Fatalf("terminal failure retained active reason: before=%s after=%s step=%+v", before.Steps[0].DisplayReason, after.Reason, after.Steps[0])
	}
}

type reviewR2RecoveryOwner struct {
	recoveredOwner
	bad   stateport.Run
	steps int
}

func (r *reviewR2RecoveryOwner) Runs(ctx context.Context, q stateport.RunFilter) ([]stateport.Run, error) {
	rows, err := r.recoveredOwner.Runs(ctx, q)
	if len(rows) > 0 {
		return append([]stateport.Run{r.bad}, rows...), err
	}
	return rows, err
}
func (r *reviewR2RecoveryOwner) Run(ctx context.Context, id string) (stateport.Run, error) {
	if id == r.bad.ID {
		return r.bad, nil
	}
	return r.recoveredOwner.Run(ctx, id)
}
func (r *reviewR2RecoveryOwner) Steps(ctx context.Context, id string) ([]stepmodel.RunStep, error) {
	r.steps++
	if id == r.bad.ID {
		return nil, errors.New("one run's projection unreadable")
	}
	return r.recoveredOwner.Steps(ctx, id)
}
func TestReviewR2OneRecoveryRowCannotStopOtherRuns(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	row, err := c.Runs.Run(t.Context(), b.RunID)
	compositionOK(t, err)
	row.ID = "broken-run"
	owner := &reviewR2RecoveryOwner{recoveredOwner: c.Runs, bad: row}
	c.Runs = owner
	f.Object.Status.Phase = wf.WorkflowSucceeded
	c.Cache = &fake.Cache{WorkflowObject: f.Object}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	reports := 0
	c.Reporter = &reportSpy{FinishedFunc: func(context.Context, string, argojob.Observation) error { reports++; cancel(); return nil }}
	err = argojob.New(c).Run(ctx)
	if reports != 1 {
		t.Fatalf("one bad run stopped entire observer: error=%v healthy_reports=%d projection_reads=%d", err, reports, owner.steps)
	}
}
