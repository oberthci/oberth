package fake

import (
	"context"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/runlog"

	core "k8s.io/api/core/v1"
)

type Cache struct {
	WorkflowObject *wf.Workflow
	PodObjects     []*core.Pod
	Changed        func(string)
}

func (f *Cache) Workflow(name string) (*wf.Workflow, bool) {
	if f.WorkflowObject == nil || f.WorkflowObject.Name != name {
		return nil, false
	}
	return f.WorkflowObject.DeepCopy(), true
}
func (f *Cache) Pods(string) []*core.Pod {
	out := []*core.Pod{}
	for _, p := range f.PodObjects {
		out = append(out, p.DeepCopy())
	}
	return out
}
func (f *Cache) OnChange(fn func(string)) { f.Changed = fn }

var _ argojob.Cache = (*Cache)(nil)

// Logs supplies reservation outcomes; retained-store tests supply real receipts.
type Logs struct {
	ReserveFunc func(context.Context, string, runlog.ContainerLog) (*runlog.Reservation, error)
	TrackFunc   func(context.Context, string, []runlog.ContainerLog) error
	KeepFunc    func(string, []runlog.ContainerLog)
}

func (f Logs) KeepFollowing(id string, entries []runlog.ContainerLog) {
	if f.KeepFunc != nil {
		f.KeepFunc(id, entries)
	}
}

func (f Logs) Track(ctx context.Context, id string, entries []runlog.ContainerLog) error {
	if f.TrackFunc != nil {
		return f.TrackFunc(ctx, id, entries)
	}
	return ctx.Err()
}

func (f Logs) Reserve(ctx context.Context, id string, entry runlog.ContainerLog) (*runlog.Reservation, error) {
	if f.ReserveFunc != nil {
		return f.ReserveFunc(ctx, id, entry)
	}
	return nil, nil
}
