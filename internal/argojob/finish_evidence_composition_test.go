package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	core "k8s.io/api/core/v1"
	"testing"
)

func TestArmedFirstTerminalPersistsEvidenceBeforeFinish(t *testing.T) {
	t.Parallel()
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "first_observation", true: "previous_running"}[stale], func(t *testing.T) { checkArmedTerminalEvidence(t, stale) })
	}
}
func checkArmedTerminalEvidence(t *testing.T, stale bool) {
	t.Helper()
	p, _, native := fixture(t, nativebinding.Preparation{Assist: true, HelperImage: "oberth.ci/helper:1"})
	s := compositionStore(t)
	registerCompositionRepository(t, s, "forge/org/repo")
	request := requestForProgress()
	request.Assist = true
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: request.ReceiveID, Repository: request.Repo, Actor: request.Actor, Runs: []stateport.Acceptance{{Request: request, Prepared: p}}})
	compositionOK(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reporter := &reportSpy{}
	c := argojob.Config{Workflows: native, Runs: s, Progress: s, Now: compositionClock, Reporter: reporter}
	e := argojob.New(c)
	_, err = e.Create(t.Context(), p)
	compositionOK(t, err)
	w := native.Object
	n := nativeNode("final", "", wf.NodeSucceeded, 1)
	n.Name = w.Name
	w.Status.Phase, w.Status.Nodes = wf.WorkflowSucceeded, wf.Nodes{n.ID: n}
	if stale {
		seedArmedRunning(t, s, p.View().RunID, n)
	}
	c.Cache = &fake.Cache{WorkflowObject: w, PodObjects: []*core.Pod{nativePod(w, n, 0)}}
	calls := 0
	reporter.FinishedFunc = func(ctx context.Context, id string, o argojob.Observation) error {
		defer cancel()
		calls++
		finishArmedNative(ctx, t, s, stateport.RunUpdate{ID: id, Status: o.Status, NativePhase: &o.Phase, ProjectCI: true})
		return nil
	}
	e = argojob.New(c)
	if err = e.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("terminal acknowledgement count", calls)
	}
}
func finishArmedNative(ctx context.Context, t *testing.T, s *store.Store, u stateport.RunUpdate) {
	t.Helper()
	nodes, err := s.Nodes(ctx, u.ID)
	compositionOK(t, err)
	if len(nodes) != 1 || nodes[0].Status != stepmodel.StepPassed {
		t.Fatal("terminal callback preceded durable final native evidence", nodes)
	}
	r, err := s.Run(ctx, u.ID)
	compositionOK(t, err)
	u.Revision = r.Revision
	compositionOK(t, s.Finish(ctx, stateport.Action{}, u))
	r, err = s.Run(ctx, u.ID)
	compositionOK(t, err)
	if r.Status != stepmodel.RunPassed || r.Origin.Assisted {
		t.Fatal("uninspected armed success lost ordinary evidence", r)
	}
}

func seedArmedRunning(t *testing.T, s *store.Store, id string, n wf.NodeStatus) {
	t.Helper()
	seq, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: 1})
	compositionOK(t, err)
	compositionOK(t, s.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: id, Sequence: seq, Nodes: []stateport.Node{{NodeID: n.ID, Path: "", Template: n.TemplateName, Attempt: 1, Status: stepmodel.StepRunning}}}))
}
