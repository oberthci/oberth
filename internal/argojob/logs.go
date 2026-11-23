package argojob

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"io"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"
)

func boundPod(w *wf.Workflow, n wf.NodeStatus, pods []*core.Pod) *core.Pod {
	var found *core.Pod
	for _, p := range pods {
		if !podOwned(w, n, p) {
			continue
		}
		if found != nil && found.UID != p.UID {
			return nil
		}
		found = p
	}
	return found
}
func podOwned(w *wf.Workflow, n wf.NodeStatus, p *core.Pod) bool {
	if p == nil || !podIdentity(w, n, p) {
		return false
	}
	owner := meta.GetControllerOf(p)
	controllers := 0
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			controllers++
		}
	}
	return controllers == 1 && owner != nil && owner.Kind == "Workflow" && owner.APIVersion == "argoproj.io/v1alpha1" && owner.Name == w.Name && owner.UID == w.UID
}

// OpenLog verifies exact Workflow, node and physical Pod before and after opening.
func (e *Executor) OpenLog(ctx context.Context, b nativebinding.RunBinding, entry runlog.ContainerLog, follow bool) (io.ReadCloser, error) {
	w, err := e.unpacked(ctx, b)
	if err != nil {
		return nil, err
	}
	n, ok := w.Status.Nodes[entry.NodeID]
	if !ok {
		return nil, ownership(nil)
	}
	p, err := e.config.Pods.Get(ctx, entry.PodName, meta.GetOptions{})
	if err != nil {
		return nil, err
	}
	if !logBinding(w, n, p, entry) {
		return nil, ownership(nil)
	}
	stream, err := e.config.Pods.Logs(ctx, p.Name, core.PodLogOptions{Container: entry.Container, Follow: follow})
	if err != nil {
		return nil, err
	}
	again, err := e.config.Pods.Get(ctx, p.Name, meta.GetOptions{})
	if err != nil || !logBinding(w, n, again, entry) {
		_ = stream.Close()
		return nil, ownership(err)
	}
	return stream, nil
}
func logBinding(w *wf.Workflow, n wf.NodeStatus, p *core.Pod, entry runlog.ContainerLog) bool {
	if !podOwned(w, n, p) || string(p.UID) != entry.PodUID || p.Name != entry.PodName || p.Namespace != entry.Namespace {
		return false
	}
	return slices.ContainsFunc(slices.Concat(p.Spec.InitContainers, p.Spec.Containers), func(c core.Container) bool { return c.Name == entry.Container })
}

// LogStore reserves a durable receipt before opening native output.
type LogStore interface {
	Reserve(context.Context, string, runlog.ContainerLog) (*runlog.Reservation, error)
	Track(context.Context, string, []runlog.ContainerLog) error
}

func (e *Executor) capture(ctx context.Context, t *tracking, w *wf.Workflow, pods []*core.Pod) {
	if e.config.Logs == nil {
		return
	}
	paths := indexPaths(w)
	jobs := map[string][]runlog.ContainerLog{}
	live := []runlog.ContainerLog{}
	nodes := slices.Collect(maps.Values(w.Status.Nodes))
	sort.Slice(nodes, func(i, j int) bool { return paths.less(nodes[i], nodes[j]) })
	for _, n := range nodes {
		p := boundPod(w, n, pods)
		if p == nil {
			continue
		}
		own := authoredContainerStatuses(nodeTemplate(w, n, admissioncontract.MaxNestingDepth), p)
		for _, c := range append(append([]core.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			entry := runlog.ContainerLog{NodeID: n.ID, Step: paths[n.ID].name, PodUID: string(p.UID), PodName: p.Name, Namespace: p.Namespace, Container: c.Name, Main: slices.ContainsFunc(own, func(status core.ContainerStatus) bool { return status.Name == c.Name })}
			if c.State.Running != nil {
				entry.State = runlog.Following
			}
			live = append(live, entry)
			if c.State.Terminated != nil {
				jobs[paths[n.ID].name] = append(jobs[paths[n.ID].name], entry)
			}
		}
	}
	if len(live) > 0 {
		if err := e.config.Logs.Track(ctx, t.binding.RunID, live); err != nil {
			e.logError(ctx, t.binding.RunID, err)
		}
	}
	for _, entries := range jobs {
		e.offerCapture(t, captureJob{binding: t.binding, entries: entries})
	}
}

func (e *Executor) captureContainer(ctx context.Context, b nativebinding.RunBinding, entry runlog.ContainerLog) error {
	key := "log/" + entry.PodUID + "/" + entry.Container
	defer e.config.RateLimiter.Forget(key)
	retry := func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.config.RateLimiter.When(key)):
			return nil
		}
	}
	return runlog.Capture(ctx, e.config.Logs, runlog.SourceCapture{RunID: b.RunID, Container: entry, Open: func(ctx context.Context) (io.ReadCloser, error) { return e.OpenLog(ctx, b, entry, false) }, Retry: retry})
}

func podIdentity(w *wf.Workflow, n wf.NodeStatus, p *core.Pod) bool {
	return p.UID != "" && p.Namespace == w.Namespace && p.Labels["workflows.argoproj.io/workflow"] == w.Name && p.Annotations["workflows.argoproj.io/node-id"] == n.ID && p.Annotations["workflows.argoproj.io/node-name"] == n.Name
}

const captureBudget, captureWorkers = 30 * time.Second, 2

type captureJob struct {
	binding nativebinding.RunBinding
	entries []runlog.ContainerLog
	until   time.Time
}

func (e *Executor) startCaptures(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range captureWorkers {
		wg.Go(func() {
			for ctx.Err() == nil {
				if job, ok := e.nextCapture(); ok {
					e.captureBatch(ctx, job)
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-e.captureReady:
				}
			}
		})
	}
	go func() { wg.Wait(); close(done) }()
	return done
}
func (e *Executor) captureBatch(ctx context.Context, job captureJob) {
	budget := captureBudget
	if !job.until.IsZero() {
		budget = min(budget, job.until.Sub(e.config.Now()))
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for _, entry := range job.entries {
		if err := e.captureContainer(ctx, job.binding, entry); err != nil {
			e.logError(ctx, job.binding.RunID, err)
		}
	}
}
func podCoverage(w *wf.Workflow, pods []*core.Pod) bool {
	for _, n := range w.Status.Nodes {
		if n.Type != wf.NodeTypePod {
			continue
		}
		p := boundPod(w, n, pods)
		if p == nil || !capturedStatuses(p) {
			return false
		}
	}
	return w.Status.OffloadNodeStatusVersion == ""
}
func capturedStatuses(p *core.Pod) bool {
	ended := map[string]bool{}
	for _, s := range append(append([]core.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		ended[s.Name] = s.State.Terminated != nil
	}
	return !slices.ContainsFunc(slices.Concat(p.Spec.InitContainers, p.Spec.Containers), func(c core.Container) bool { return !ended[c.Name] })
}
