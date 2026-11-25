package argojob_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

func TestReceiptWriteFailurePublishesUnknownThroughIndependentHealth(t *testing.T) {
	for _, kind := range []string{"source", "secret"} {
		t.Run(kind, func(t *testing.T) { checkLostDeliveryEvidence(t, kind) })
	}
}

func checkLostDeliveryEvidence(t *testing.T, kind string) {
	t.Helper()
	c, f, b := observationFixture(t)
	n := nativeNode("one", "", wf.NodeFailed, 100)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase, f.Object.Status.Nodes = wf.WorkflowFailed, wf.Nodes{n.ID: n}
	p := receiptPod(f.Object, n, kind)
	c.Cache = &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{p}}
	directory := t.TempDir()
	logs, err := runlog.Open(t.Context(), directory, 4096)
	compositionOK(t, err)
	defer func() { _ = logs.Close() }()
	r := runlog.InfrastructureReceipt{RunID: b.RunID, WorkflowUID: string(f.Object.UID), NodeID: n.ID, PodUID: string(p.UID), Kind: kind, StartedAt: n.StartedAt.Time, FinishedAt: n.FinishedAt.Time}
	compositionOK(t, os.Chmod(directory, 0500))
	t.Cleanup(func() { _ = os.Chmod(directory, 0700) })
	if err = logs.RecordInfrastructure(t.Context(), r); err == nil {
		t.Fatal("receipt write failure was not injected")
	}
	// Restore disk writes before observing. Lost evidence still cannot become
	// proof of an author failure just because the volume is healthy again.
	compositionOK(t, os.Chmod(directory, 0700))
	c.Infrastructure = logs
	var diagnostic bytes.Buffer
	c.Logger = slog.New(slog.NewTextHandler(&diagnostic, nil))
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	reports := 0
	c.Reporter = &reportSpy{FinishedFunc: func(_ context.Context, _ string, out argojob.Observation) error {
		reports++
		if out.Failure == nil || out.Failure.Class != stepmodel.FailureUnknown || out.Failure.Retry.Action != stepmodel.RetryNone || out.Steps[0].DisplayReason != "delivery evidence unavailable" {
			t.Error("lost receipt became author or retryable infrastructure", out.Failure, out.Steps)
		}
		cancel()
		return nil
	}}
	_ = argojob.New(c).Run(ctx)
	if reports != 1 || !strings.Contains(diagnostic.String(), "level=ERROR") {
		t.Fatal("lost evidence blocked the final observation or was hidden", reports, diagnostic.String())
	}
	steps, err := c.Runs.Steps(t.Context(), b.RunID)
	if err != nil || len(steps) != 1 || steps[0].FailureClass != stepmodel.FailureUnknown {
		t.Fatal("unknown projection was not retained", steps, err)
	}
}

func TestUnknownDeliveryEvidenceReplacesOnlyDeliveryAttribution(t *testing.T) {
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowFailed
	n := nativeNode("one", ".build", wf.NodeFailed, 100)
	w.Status.Nodes[n.ID] = n
	p := receiptPod(w, n, "secret")
	r := runlog.InfrastructureReceipt{RunID: "run", WorkflowUID: string(w.UID), NodeID: n.ID, Step: "build", PodUID: string(p.UID), Kind: "secret", StartedAt: n.StartedAt.Time, FinishedAt: n.FinishedAt.Time}
	in := argojob.Projection{RunID: "run", Workflow: w, Pods: []*core.Pod{p}, Plan: admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, Infrastructure: []runlog.InfrastructureReceipt{r}}
	before := project(t, in)
	in.Previous, in.PreviousNodes, in.DeliveryEvidenceUnavailable = before.Steps, before.Nodes, true
	after := project(t, in)
	if after.Failure == nil || after.Failure.Class != stepmodel.FailureUnknown {
		t.Fatal("lost success tombstone retained stale delivery infrastructure", after.Failure)
	}
	p.Status.Reason = "NodeShutdown"
	after = project(t, in)
	if after.Failure == nil || after.Failure.Class != stepmodel.FailureInfrastructure || after.Steps[0].DisplayReason != "node shut down" {
		t.Fatal("lost delivery evidence erased independent native evidence", after.Failure)
	}
}
