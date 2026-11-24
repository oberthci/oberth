package argojob_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type collectionNative struct {
	*fake.Workflows
	collected func(*wf.Workflow)
}

func (n collectionNative) Patch(ctx context.Context, patch argojob.PatchRequest) (*wf.Workflow, error) {
	w, err := n.Workflows.Patch(ctx, patch)
	if err == nil && !slices.Contains(w.Finalizers, nativebinding.RecordFinalizer) {
		n.collected(w)
	}
	return w, err
}

func TestNativeCollectionWaitsForDurableFinishAndKeepsRunAndLogs(t *testing.T) {
	t.Parallel()
	s, f, b, pod := collectionFixture(t)
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	compositionOK(t, err)
	defer func() { compositionOK(t, logs.Close()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	patches := 0
	native := collectionNative{Workflows: f, collected: func(w *wf.Workflow) {
		patches++
		assertCollectedHistory(t, s, logs, b.RunID)
		f.Object = nil // Kubernetes finishes a pending delete when its last finalizer goes.
		cancel()
	}}
	reports := 0
	config := argojob.Config{Runs: s, Progress: s, Logs: logs, Workflows: native, Pods: &fake.Pods{Object: pod}, Cache: &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}, Now: compositionClock}
	config.Reporter = collectionReporter(t, s, f, &reports)
	_ = argojob.New(config).Run(ctx)
	if reports < 2 || patches != 1 || f.Object != nil {
		t.Fatalf("collection did not follow durable finish: reports=%d patches=%d context=%v", reports, patches, ctx.Err())
	}
	assertCollectedHistory(t, s, logs, b.RunID)
}

func collectionFixture(t *testing.T) (*store.Store, *fake.Workflows, nativebinding.RunBinding, *core.Pod) {
	t.Helper()
	s := compositionStore(t)
	r := requestForProgress()
	registerCompositionRepository(t, s, r.Repo)
	p := compositionAdmission(t, "18551855185518551855185518551855", r)
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}})
	compositionOK(t, err)
	f := &fake.Workflows{}
	b, err := argojob.New(argojob.Config{Runs: s, Workflows: f}).Create(t.Context(), p)
	compositionOK(t, err)
	if f.Object.Spec.TTLStrategy == nil || *f.Object.Spec.TTLStrategy.SecondsAfterCompletion != 86400 || !slices.Contains(f.Object.Finalizers, nativebinding.RecordFinalizer) {
		t.Fatal("native TTL is missing or deletion is not protected", f.Object)
	}
	n := nativeNode("one", ".build", wf.NodeSucceeded, 1)
	n.Name = b.Workflow.Name + ".build"
	f.Object.Status.Phase, f.Object.Status.Nodes = wf.WorkflowSucceeded, wf.Nodes{n.ID: n}
	// Model a TTL delete requested while the server could not commit Finish.
	deletion := meta.NewTime(compositionClock().Add(24 * time.Hour))
	f.Object.DeletionTimestamp = &deletion
	pod := nativePod(f.Object, n, 0)
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, core.ContainerStatus{Name: "wait", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{}}})
	return s, f, b, pod
}

func collectionReporter(t *testing.T, s *store.Store, f *fake.Workflows, calls *int) *reportSpy {
	t.Helper()
	return &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		*calls++
		if !slices.Contains(f.Object.Finalizers, nativebinding.RecordFinalizer) {
			t.Error("Workflow became collectible before Finish")
		}
		if *calls == 1 {
			return errors.New("durable Finish unavailable")
		}
		r, err := s.Run(ctx, id)
		if err != nil {
			return err
		}
		return s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: r.Revision, Status: o.Status, NativePhase: &o.Phase, Reason: o.Reason, Detail: o.Detail, Failure: o.Failure})
	}}
}

func assertCollectedHistory(t *testing.T, s *store.Store, logs *runlog.Store, id string) {
	t.Helper()
	r, err := s.Run(t.Context(), id)
	compositionOK(t, err)
	steps, err := s.Steps(t.Context(), id)
	compositionOK(t, err)
	if r.Status != stepmodel.RunPassed || !r.EvidenceComplete || len(steps) != 1 || steps[0].Status != stepmodel.StepPassed {
		t.Fatal("final record and steps were not durable before deletion", r, steps)
	}
	body, _, err := logs.Read(t.Context(), id, runlog.ReadRequest{OwnOnly: true})
	if err != nil || string(body) != "container output\n" {
		t.Fatal("retained logs unavailable across native deletion", string(body), err)
	}
}
