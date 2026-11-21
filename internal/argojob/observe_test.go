package argojob_test

import (
	"context"
	"fmt"
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

func TestObserverWritesOneDeclaredEvidenceSequenceAndFinishesBeforeEvidence(t *testing.T) {
	t.Parallel()
	for _, count := range []int{128, 129} {
		t.Run(fmt.Sprint(count), func(t *testing.T) { checkDeclaredSequence(t, count) })
	}
}
func checkDeclaredSequence(t *testing.T, count int) {
	t.Helper()
	c, f, b := observationFixture(t)
	s := c.Runs.(*statefake.Runs)
	f.Object.Status.Phase = wf.WorkflowSucceeded
	f.Object.Status.Nodes = wf.Nodes{}
	retry := wf.NodeStatus{ID: "retry", Name: b.Workflow.Name, Type: wf.NodeTypeRetry}
	pods := []*core.Pod{}
	for i := range count {
		n := nativeNode(fmt.Sprint(i), fmt.Sprintf("(%d)", i), wf.NodeSucceeded, int64(i+1))
		n.Name = b.Workflow.Name + fmt.Sprintf("(%d)", i)
		retry.Children = append(retry.Children, n.ID)
		f.Object.Status.Nodes[n.ID] = n
		pods = append(pods, nativePod(f.Object, n, 0))
	}
	f.Object.Status.Nodes[retry.ID] = retry
	c.Cache = &fake.Cache{WorkflowObject: f.Object, PodObjects: pods}
	finished, begin := 0, 0
	c.Reporter = &reportSpy{FinishedFunc: func(_ context.Context, _ string, o argojob.Observation) error {
		finished++
		if begin != 0 || !o.Complete || o.Status != stepmodel.RunPassed {
			t.Error("terminal report waited for evidence", o)
		}
		return nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c.Progress = &progressProbe{Runs: s, begin: func(ctx context.Context, a stateport.Action, in stateport.EvidenceStart) (int64, error) {
		begin++
		return s.BeginEvidence(ctx, a, in)
	}, complete: func(ctx context.Context, a stateport.Action, in stateport.EvidenceCompletion) error {
		err := s.CompleteEvidence(ctx, a, in)
		cancel()
		return err
	}}
	_ = argojob.New(c).Run(ctx)
	nodes, err := s.Nodes(t.Context(), b.RunID)
	compositionOK(t, err)
	r, err := s.Run(t.Context(), b.RunID)
	compositionOK(t, err)
	if finished != 1 || begin != 1 || len(nodes) != count || !r.EvidenceComplete {
		t.Fatalf("finished=%d begin=%d nodes=%d complete=%v", finished, begin, len(nodes), r.EvidenceComplete)
	}
}
