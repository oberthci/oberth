package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/runlog"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureReservationRefusalRetriesWithObservationBackoff(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	n := nativeNode("one", "", wf.NodeSucceeded, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowRunning
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	c.Cache = &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{nativePod(f.Object, n, 0)}}
	clock := time.Unix(1, 0)
	limiter := &advancingBackoff{TypedRateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, 8*time.Second), now: &clock}
	c.RateLimiter = limiter
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	attempts := 0
	c.Logs = fake.Logs{ReserveFunc: func(context.Context, string, runlog.ContainerLog) (*runlog.Reservation, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("log store temporarily refused")
		}
		cancel()
		return nil, nil
	}}
	_ = argojob.New(c).Run(ctx)
	if attempts != 3 || len(limiter.delays) != 2 || limiter.delays[0] != time.Second || limiter.delays[1] != 2*time.Second {
		t.Fatal(attempts, limiter.delays)
	}
}

type repairingBackoff struct {
	workqueue.TypedRateLimiter[string]
	repair func()
	calls  int
}

func (l *repairingBackoff) When(string) time.Duration { l.calls++; l.repair(); return time.Nanosecond }
func TestNativeCaptureRetriesCommitWithSamePhysicalSource(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	n := nativeNode("one", "", wf.NodeSucceeded, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowRunning
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	pod := nativePod(f.Object, n, 0)
	pods := &fake.Pods{Object: pod}
	c.Pods = pods
	c.Cache = &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}
	root := t.TempDir()
	logs, err := runlog.Open(t.Context(), root, 4096)
	compositionOK(t, err)
	t.Cleanup(func() { compositionOK(t, logs.Close()) })
	c.Logs = logs
	path := filepath.Join(root, b.RunID+".log")
	compositionOK(t, os.Mkdir(path, 0700))
	limiter := &repairingBackoff{TypedRateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, 8*time.Second), repair: func() { compositionOK(t, os.Remove(path)) }}
	c.RateLimiter = limiter
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_ = argojob.New(c).Run(ctx)
	body, _, err := logs.Read(t.Context(), b.RunID, runlog.ReadRequest{})
	compositionOK(t, err)
	if limiter.calls != 1 || pods.Opens != 1 || string(body) != "container output\n" {
		t.Fatal("capture retry reopened or lost its source", limiter.calls, pods.Opens, string(body))
	}
}
