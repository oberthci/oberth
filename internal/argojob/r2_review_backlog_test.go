package argojob

import (
	"context"
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"io"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"log/slog"
	"runtime"
	"sync/atomic"
	"testing"
)

type reviewBlockedLogStore struct {
	entered      chan struct{}
	active, peak atomic.Int32
}

func (s *reviewBlockedLogStore) Reserve(ctx context.Context, _ string, _ runlog.ContainerLog) (*runlog.Reservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for old := s.peak.Load(); n > old && !s.peak.CompareAndSwap(old, n); old = s.peak.Load() {
	}
	s.entered <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestReviewR1CaptureBacklogResources(t *testing.T) {
	t.Parallel()
	logs := &reviewBlockedLogStore{entered: make(chan struct{}, 2)}
	e := New(Config{Logs: logs, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(t.Context())
	done := e.startCaptures(ctx)
	defer func() { cancel(); e.queue.ShutDown(); <-done }()
	for i := range 2000 {
		name := fmt.Sprintf("run-%04d", i)
		w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: "ci", UID: types.UID(name)}, Status: wf.WorkflowStatus{Nodes: wf.Nodes{}}}
		n := wf.NodeStatus{ID: "node", Name: name + ".build", Type: wf.NodeTypePod, Phase: wf.NodeSucceeded}
		w.Status.Nodes[n.ID] = n
		p := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod-" + name, Namespace: "ci", UID: types.UID("pod-" + name), Labels: map[string]string{"workflows.argoproj.io/workflow": name}, Annotations: map[string]string{"workflows.argoproj.io/node-id": n.ID, "workflows.argoproj.io/node-name": n.Name}, OwnerReferences: []meta.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: name, UID: w.UID, Controller: new(true)}}}, Spec: core.PodSpec{Containers: []core.Container{{Name: "main"}}}, Status: core.PodStatus{ContainerStatuses: []core.ContainerStatus{{Name: "main", State: core.ContainerState{Terminated: &core.ContainerStateTerminated{}}}}}}
		tr := &tracking{binding: nativebinding.RunBinding{RunID: name, Workflow: nativebinding.Object{Name: name}}, captures: map[string]bool{}}
		e.mu.Lock()
		e.tracked[name] = tr
		e.mu.Unlock()
		e.capture(t.Context(), tr, w, []*core.Pod{p})
		if i < 2 {
			<-logs.entered
		}
		e.Untrack(name)
		if (i+1)%200 == 0 {
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			t.Logf("completed_runs=%d active_tracks=%d active_capture=%d queued_capture=%d heap_live=%d", i+1, len(e.tracked), logs.active.Load(), pendingCaptures(e), m.HeapAlloc)
		}
	}
	if logs.peak.Load() != 2 || pendingCaptures(e) != 0 {
		t.Fatal(logs.peak.Load(), pendingCaptures(e))
	}
}

func pendingCaptures(e *Executor) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := 0
	for _, tr := range e.tracked {
		count += len(tr.pending)
	}
	return count
}

func (*reviewBlockedLogStore) Track(context.Context, string, []runlog.ContainerLog) error { return nil }
func (*reviewBlockedLogStore) KeepFollowing(string, []runlog.ContainerLog)                {}
