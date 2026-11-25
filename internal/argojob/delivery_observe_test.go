package argojob_test

import (
	"context"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

func TestSourceReceiptConfirmsLiveInitWhenPodCacheLags(t *testing.T) {
	checkLiveDeliveryPod(t, false)
}

func TestSourceReceiptRejectsReplacedLivePod(t *testing.T) {
	checkLiveDeliveryPod(t, true)
}

func checkLiveDeliveryPod(t *testing.T, replaced bool) {
	c, f, b := observationFixture(t)
	n := nativeNode("one", ".build", wf.NodeFailed, 100)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowFailed
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	p := receiptPod(f.Object, n, "source")
	lagging := p.DeepCopy()
	lagging.Status.InitContainerStatuses = nil
	lagging.Status.ContainerStatuses[0].State = core.ContainerState{Waiting: &core.ContainerStateWaiting{Reason: "PodInitializing"}}
	c.Cache = &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{lagging}}
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	compositionOK(t, err)
	defer func() { _ = logs.Close() }()
	c.Infrastructure = logs
	r := runlog.InfrastructureReceipt{RunID: b.RunID, WorkflowUID: string(f.Object.UID), NodeID: n.ID, PodUID: string(p.UID), Kind: "source", StartedAt: n.StartedAt.Time, FinishedAt: n.FinishedAt.Time}
	compositionOK(t, logs.RecordInfrastructure(t.Context(), r))
	reads := 0
	c.Pods = &fake.Pods{GetFunc: func() (*core.Pod, error) {
		reads++
		live := p.DeepCopy()
		if replaced {
			live.UID = "replacement"
		}
		return live, nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	reports := 0
	c.Reporter = &reportSpy{FinishedFunc: func(_ context.Context, _ string, out argojob.Observation) error {
		reports++
		if replaced {
			t.Error("replacement Pod published the old terminal result")
		}
		if reads != 1 || out.Failure == nil || out.Failure.Class != stepmodel.FailureInfrastructure || out.Failure.Retry.Action != stepmodel.RetryRerun {
			t.Error("lagging init lost terminal source receipt", reads, out.Failure)
		}
		cancel()
		return nil
	}}
	_ = argojob.New(c).Run(ctx)
	want := 1
	if replaced {
		want = 0
	}
	if reports != want || reads == 0 {
		t.Fatal("source failure was not reported", reports)
	}
}
