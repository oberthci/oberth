package argojob_test

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	core "k8s.io/api/core/v1"
	"testing"
	"time"
)

type nativeCompletionProbe struct {
	*store.Store
	done context.CancelFunc
}

func (p nativeCompletionProbe) CompleteEvidence(ctx context.Context, a stateport.Action, c stateport.EvidenceCompletion) error {
	err := p.Store.CompleteEvidence(ctx, a, c)
	p.done()
	return err
}
func TestFullObserverComposesRealStoreWithoutAlternateProjection(t *testing.T) {
	t.Parallel()
	s := compositionStore(t)
	r := requestForProgress()
	registerCompositionRepository(t, s, r.Repo)
	p := compositionAdmission(t, "observer-real", r)
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}})
	compositionOK(t, err)
	f := &fake.Workflows{}
	b, err := argojob.New(argojob.Config{Runs: s, Workflows: f}).Create(t.Context(), p)
	compositionOK(t, err)
	if b.Workflow.Name != nativebinding.WorkflowName(p.View().RunID) || f.Object.GenerateName != "" {
		t.Fatal("native name differs", b)
	}
	n := nativeNode("one", ".build", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name + ".build"
	f.Object.Status.Phase = wf.WorkflowFailed
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	reports := 0
	c := argojob.Config{Runs: s, Progress: nativeCompletionProbe{Store: s, done: cancel}, Workflows: f, Cache: &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{nativePod(f.Object, n, 7)}}, Now: compositionClock}
	c.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		reports++
		before, err := s.Run(ctx, id)
		if err != nil {
			return err
		}
		return s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: before.Revision, Status: o.Status, NativePhase: &o.Phase, Reason: o.Reason, Detail: o.Detail, Failure: o.Failure})
	}}
	_ = argojob.New(c).Run(ctx)
	got, err := s.Run(t.Context(), b.RunID)
	compositionOK(t, err)
	steps, err := s.Steps(t.Context(), b.RunID)
	compositionOK(t, err)
	nodes, err := s.Nodes(t.Context(), b.RunID)
	compositionOK(t, err)
	if reports != 1 || !got.EvidenceComplete || got.Failed != 1 || len(steps) != 1 || steps[0].DisplayReason != "exited with code 7" || len(steps[0].Logs) != 1 || len(nodes) != 1 || nodes[0].Path != "build" {
		t.Fatalf("real composition differs: context=%v reports=%d complete=%t failed=%d steps=%+v nodes=%+v", ctx.Err(), reports, got.EvidenceComplete, got.Failed, steps, nodes)
	}
}
