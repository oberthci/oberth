package argojob_test

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
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
)

func TestCancelledBeforePodStartCanBeCollected(t *testing.T) {
	t.Parallel()
	for _, scheduled := range []bool{false, true} {
		db, f, b, _ := collectionFixture(t)
		n := f.Object.Status.Nodes["one"]
		n.Phase = wf.NodeFailed
		if scheduled {
			n.HostNodeName = "old-node"
		}
		f.Object.Status.Nodes[n.ID] = n
		f.Object.Status.Phase, f.Object.Spec.Shutdown = wf.WorkflowFailed, wf.ShutdownStrategyTerminate
		r, err := db.Run(t.Context(), b.RunID)
		compositionOK(t, err)
		compositionOK(t, db.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunCancelled}))
		before := collectionHistory(t, db, b.RunID)
		logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
		compositionOK(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		collected := false
		native := collectionNative{Workflows: f, collected: func(*wf.Workflow) { collected = true; cancel() }}
		cache := &fake.Cache{WorkflowObject: f.Object}
		e := argojob.New(argojob.Config{Runs: db, Progress: db, Logs: logs, Workflows: native, Pods: &fake.Pods{}, Cache: cache, Now: compositionClock})
		cache.Changed(b.Workflow.Name)
		_ = e.Run(ctx)
		cancel()
		receipts, err := logs.Containers(t.Context(), b.RunID)
		compositionOK(t, err)
		want := "no log: step never ran"
		if scheduled {
			want = "pod missing"
		}
		if !collected || len(receipts) != 1 || receipts[0].State != runlog.Captured || receipts[0].Reason != want {
			t.Fatal("terminal Pod absence blocked collection or lost its durable gap", collected, receipts)
		}
		if string(before) != string(collectionHistory(t, db, b.RunID)) {
			t.Fatal("collection rewrote cancelled history")
		}
		compositionOK(t, logs.Close())
	}
}

func TestHistoricalCollectionLeavesFinishedRunBytesUnchanged(t *testing.T) {
	t.Parallel()
	db, f, b, pod := collectionFixture(t)
	compositionOK(t, db.SaveProgress(t.Context(), b.RunID, stateport.Progress{Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepPassed, DisplayReason: "recorded before upgrade"}}}))
	sequence, err := db.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: b.RunID, Nodes: 1})
	compositionOK(t, err)
	compositionOK(t, db.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: b.RunID, Sequence: sequence, Nodes: []stateport.Node{{NodeID: "old-node", Path: "build", Template: "build", PodName: "old-pod", Attempt: 1, Status: stepmodel.StepPassed, Exit: new(0), Credential: new(false)}}}))
	compositionOK(t, db.CompleteEvidence(t.Context(), stateport.Action{}, stateport.EvidenceCompletion{RunID: b.RunID, Sequence: sequence}))
	r, err := db.Run(t.Context(), b.RunID)
	compositionOK(t, err)
	compositionOK(t, db.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
	f.Object.Spec.TTLStrategy, f.Object.Finalizers = nil, nil
	before := collectionHistory(t, db, b.RunID)
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	compositionOK(t, err)
	defer func() { _ = logs.Close() }()
	entry := runlog.ContainerLog{NodeID: "one", Step: "old/log/path", Namespace: pod.Namespace, PodName: pod.Name, PodUID: string(pod.UID), Container: "main"}
	reservation, err := logs.Reserve(t.Context(), b.RunID, entry)
	compositionOK(t, err)
	compositionOK(t, reservation.Capture(t.Context(), runlog.CaptureRequest{Source: io.NopCloser(strings.NewReader("recorded main output\n"))}))
	oldReceipts, err := logs.Containers(t.Context(), b.RunID)
	compositionOK(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	collected := false
	native := collectionNative{Workflows: f, collected: func(w *wf.Workflow) {
		collected = true
		if slices.Contains(w.Finalizers, nativebinding.RecordFinalizer) {
			t.Error("archive guard retained after commit")
		}
		cancel()
	}}
	cache := &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}
	e := argojob.New(argojob.Config{Runs: db, Progress: db, Logs: logs, Workflows: native, Pods: &fake.Pods{Object: pod}, Cache: cache, Now: compositionClock})
	cache.Changed(b.Workflow.Name)
	_ = e.Run(ctx)
	if !collected || string(before) != string(collectionHistory(t, db, b.RunID)) {
		t.Fatal("backfill rewrote a finished run, steps or evidence", collected)
	}
	afterReceipts, err := logs.Containers(t.Context(), b.RunID)
	compositionOK(t, err)
	for _, receipt := range afterReceipts {
		if receipt.Container == "main" && receipt != oldReceipts[0] {
			t.Fatal("backfill rewrote retained log attribution", receipt, oldReceipts[0])
		}
	}
}

func collectionHistory(t *testing.T, db *store.Store, id string) []byte {
	t.Helper()
	r, err := db.Run(t.Context(), id)
	compositionOK(t, err)
	steps, err := db.Steps(t.Context(), id)
	compositionOK(t, err)
	nodes, err := db.Nodes(t.Context(), id)
	compositionOK(t, err)
	raw, err := json.Marshal(struct {
		Run   stateport.Run
		Steps []stepmodel.RunStep
		Nodes []stateport.Node
	}{r, steps, nodes})
	compositionOK(t, err)
	return raw
}
