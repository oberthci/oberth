package argojob_test

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"testing"
)

func TestRestartThroughNativeFinish(t *testing.T) {
	t.Parallel()
	for _, tc := range []conditionalEvidenceCase{
		{name: "armed-restarted-success", capture: "restart-held-status-lost", restarted: true, excluded: true},
		{name: "unarmed-restarted-success", capture: "restart-held-status-lost", restarted: true, unarmed: true, excluded: false},
		{name: "armed-restarted-before-start", capture: "restart-infra-before-start", restarted: true, excluded: true},
		{name: "retained-restart", capture: "restart-container", change: "prior-restart", restarted: true, excluded: true},
		{name: "armed-success", capture: "restart-container", restarted: false, excluded: false},
		{name: "restart-disabled-failure", capture: "restart-held-status-lost-disabled", restarted: false, excluded: true},
		{name: "when-skipped-no-restart", capture: "when-skipped", restarted: false, excluded: false},
	} {
		t.Run(tc.name, func(t *testing.T) { conditionalFinish(t, tc) })
	}
}

// The unarmed path writes evidence after finish; stop only once it is durable.
type cancelAfterRestartEvidence struct {
	*store.Store
	cancel context.CancelFunc
}

func (s cancelAfterRestartEvidence) PutNodes(ctx context.Context, a stateport.Action, b stateport.NodeBatch) error {
	err := s.Store.PutNodes(ctx, a, b)
	s.cancel()
	return err
}

func TestRestartSurvivesNativeObservation(t *testing.T) {
	t.Parallel()
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowSucceeded
	n := nativeNode("build", ".build", wf.NodeSucceeded, 1)
	n.FailedPodRestarts = 1
	w.Status.Nodes[n.ID] = n
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build", HoldArmed: true}}}
	first := project(t, argojob.Projection{Plan: plan, Workflow: w})
	if len(first.Nodes) != 1 || !first.Nodes[0].PodRestarted {
		t.Fatal("positive native restart missing", first.Nodes)
	}
	n.FailedPodRestarts = 0
	w.Status.Nodes[n.ID] = n
	final := project(t, argojob.Projection{Plan: plan, Workflow: w, PreviousNodes: first.Nodes})
	if len(final.Nodes) != 1 || !final.Nodes[0].PodRestarted {
		t.Fatal("native restart observation was erased", final.Nodes)
	}
}
