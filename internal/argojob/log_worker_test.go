package argojob_test

import (
	"context"
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type snapshotCache struct {
	mu      sync.Mutex
	w       *wf.Workflow
	pods    []*core.Pod
	changed func(string)
}

func (c *snapshotCache) Workflow(string) (*wf.Workflow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.DeepCopy(), true
}
func (c *snapshotCache) Pods(string) []*core.Pod {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*core.Pod{}, c.pods...)
}
func (c *snapshotCache) OnChange(fn func(string)) { c.changed = fn }
func (c *snapshotCache) terminal() {
	c.mu.Lock()
	c.w.Status.Phase = wf.WorkflowSucceeded
	name := c.w.Name
	c.mu.Unlock()
	c.changed(name)
}
func TestSlowLogCaptureDoesNotBlockTerminalObservation(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	f.Object.Status.Phase = wf.WorkflowRunning
	f.Object.Status.Nodes = wf.Nodes{}
	retry := wf.NodeStatus{ID: "retry", Name: b.Workflow.Name, Type: wf.NodeTypeRetry}
	pods := []*core.Pod{}
	for i := range 3 {
		n := nativeNode(fmt.Sprint(i), fmt.Sprintf("(%d)", i), wf.NodeSucceeded, int64(i))
		n.Name = b.Workflow.Name + fmt.Sprintf("(%d)", i)
		retry.Children = append(retry.Children, n.ID)
		f.Object.Status.Nodes[n.ID] = n
		pods = append(pods, nativePod(f.Object, n, 0))
	}
	f.Object.Status.Nodes[retry.ID] = retry
	cache := &snapshotCache{w: f.Object.DeepCopy(), pods: pods}
	c.Cache = cache
	started := make(chan struct{}, 3)
	var active, peak atomic.Int32
	c.Logs = fake.Logs{ReserveFunc: func(ctx context.Context, _ string, _ runlog.ContainerLog) (*runlog.Reservation, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); count > old && !peak.CompareAndSwap(old, count); old = peak.Load() {
		}
		_, ok := ctx.Deadline()
		if !ok {
			t.Error("unbounded log capture")
		}
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reported := false
	c.Reporter = &reportSpy{FinishedFunc: func(_ context.Context, _ string, o argojob.Observation) error {
		reported = o.Status == stepmodel.RunPassed
		cancel()
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- argojob.New(c).Run(ctx) }()
	for range 1 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("log workers did not start")
		}
	}
	cache.terminal()
	<-done
	if !reported || peak.Load() > 2 || peak.Load() < 1 {
		t.Fatal("logs blocked status or exceeded workers", reported, peak.Load())
	}
}
