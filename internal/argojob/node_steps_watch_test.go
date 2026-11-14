package argojob

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type notifyingNodeWriter struct {
	*store.Store
	written chan error
}

func (w notifyingNodeWriter) PutStepResults(ctx context.Context, runID string, rows []model.StepResult) error {
	err := w.Store.PutStepResults(ctx, runID, rows)
	w.written <- err
	return err
}

func TestWatchWaitStoresStepsWhileRunIsRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db, err := storetest.Open(ctx, filepath.Join(t.TempDir(), "nodes.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	upstream, err := storetest.CreateUpstream(db, ctx, model.UpstreamSpec{Name: "forge", Kind: "forgejo", BaseURL: "https://forge.example"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := storetest.EnqueueRun(db, ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "nodes", SHA: strings.Repeat("a", 40), Actor: "reader", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.StartRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	wf := watchWaitWorkflow(wfv1.WorkflowRunning)
	wf.Annotations[runIDAnnotation] = run.ID
	wf.Status.Nodes = wfv1.Nodes{"node": {ID: "node", DisplayName: "leaf", TemplateName: "leaf", Type: wfv1.NodeTypePod, Phase: wfv1.NodeRunning}}
	hub := newControlledWaitHub(wf)
	writer := notifyingNodeWriter{Store: db, written: make(chan error, 4)}
	c := (&Controller{config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}}).WithWatchHub(hub).WithStepResultWriter(writer)
	result := startControlledWait(ctx, c, wf)
	defer func() { cancel(); <-result }()
	select {
	case err = <-writer.written:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	rows, err := db.StepResults(ctx, run.ID)
	if err != nil || len(rows) != 1 || rows[0].NodeID != "node" || rows[0].Status != model.StepRunning {
		t.Fatal("running node absent from history", rows, err)
	}
	active, err := db.Run(ctx, run.ID)
	if err != nil || active.Status != model.RunRunning {
		t.Fatal("steps were stored only at completion", active.Status, err)
	}
	assertStillWaiting(t, result)
	next := wf.DeepCopy()
	node := next.Status.Nodes["node"]
	node.Phase = wfv1.NodeSucceeded
	next.Status.Nodes["node"] = node
	hub.setWorkflow(next)
	select {
	case err = <-writer.written:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	rows, err = db.StepResults(ctx, run.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != model.StepPassed {
		t.Fatal("watch transition was not retained", rows, err)
	}
}

type changedNodeWriter struct {
	batches [][]model.StepResult
	fail    atomic.Bool
}

func (w *changedNodeWriter) PutStepResults(_ context.Context, _ string, rows []model.StepResult) error {
	w.batches = append(w.batches, append([]model.StepResult(nil), rows...))
	if w.fail.Load() {
		return errors.New("history write unavailable")
	}
	return nil
}

func TestWatchWaitWritesOnlyChangedNodeRows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wf := watchWaitWorkflow(wfv1.WorkflowRunning)
		wf.Status.Nodes = wfv1.Nodes{
			"one": {ID: "one", DisplayName: "same", Phase: wfv1.NodeRunning},
			"two": {ID: "two", DisplayName: "same", Phase: wfv1.NodeRunning},
		}
		hub := newControlledWaitHub(wf)
		writer := &changedNodeWriter{}
		c := (&Controller{config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}}).WithWatchHub(hub).WithStepResultWriter(writer)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := startControlledWait(ctx, c, wf)
		synctest.Wait()
		if len(writer.batches) != 1 || len(writer.batches[0]) != 2 {
			t.Fatal(writer.batches)
		}
		hub.setWorkflow(wf.DeepCopy())
		synctest.Wait()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if len(writer.batches) != 1 {
			t.Fatal("unchanged events or idle clocks rewrote history", writer.batches)
		}
		changed := wf.DeepCopy()
		node := changed.Status.Nodes["two"]
		node.Phase = wfv1.NodeFailed
		node.Message = "\x1b[31mfailed\x1b[0m\n"
		changed.Status.Nodes["two"] = node
		writer.fail.Store(true)
		hub.setWorkflow(changed)
		synctest.Wait()
		writer.fail.Store(false)
		time.Sleep(waitIdleInterval)
		synctest.Wait()
		if len(writer.batches) != 3 || len(writer.batches[1]) != 1 || len(writer.batches[2]) != 1 || writer.batches[2][0].NodeID != "two" || writer.batches[2][0].Message != "failed" {
			t.Fatal("changed row lost or failed write was not retried", writer.batches)
		}
		cancel()
		<-result
	})
}
