package argojob_test

import (
	"context"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestQueuedSubmissionShowsItsLatestSlotAndLockWait(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	w := f.Object
	w.Status.Phase = ""
	c.Cache = &fake.Cache{WorkflowObject: w}
	s := c.Runs.(*statefake.Runs)
	for _, reason := range []string{"waiting to start", "waiting for a run slot", "waiting for sync limit heavy-go"} {
		switch reason {
		case "waiting for a run slot":
			w.Status.Phase = wf.WorkflowPending
			w.Status.Message = "Workflow processing has been postponed because too many workflows are already running"
		case "waiting for sync limit heavy-go":
			w.Status.Phase, w.Status.Message = wf.WorkflowRunning, ""
			w.Status.Nodes = wf.Nodes{"retry": {ID: "retry", Name: w.Name, Type: wf.NodeTypeRetry, Phase: wf.NodePending, SynchronizationStatus: &wf.NodeSynchronizationStatus{Waiting: "ci/ConfigMap/oberth-sync-limits/heavy-go"}}}
		}
		observeQueuedProgress(t, c)
		r, err := s.Run(t.Context(), b.RunID)
		compositionOK(t, err)
		steps, err := s.Steps(t.Context(), b.RunID)
		compositionOK(t, err)
		if r.Status != stepmodel.RunQueued || r.DisplayReason != reason || len(steps) != 1 || steps[0].DisplayReason != reason {
			t.Fatalf("submission wait differs: run %s / %q, steps %+v, want %q", r.Status, r.DisplayReason, steps, reason)
		}
	}
}

// Each observation uses a fresh executor so only the stored run carries progress.
func observeQueuedProgress(t *testing.T, c argojob.Config) {
	t.Helper()
	s := c.Runs.(*statefake.Runs)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	writes := 0
	c.Progress = &progressProbe{Runs: s, save: func(ctx context.Context, id string, p stateport.Progress) error {
		err := s.SaveProgress(ctx, id, p)
		writes++
		cancel()
		return err
	}}
	_ = argojob.New(c).Run(ctx)
	if writes != 1 {
		t.Fatalf("observation saved %d times", writes)
	}
}
